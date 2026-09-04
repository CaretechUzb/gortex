// Package forge is the single network surface for pull-request I/O. It offers
// a small set of free functions — ListPRs, ViewPR, PRFiles, DiffPR,
// PostReviewComments, Available — every one taking (ctx, repoDir, …).
//
// Two backends sit behind them: ghClient (GitHub and GitHub Enterprise, on the
// official go-github SDK) and glClient (gitlab.com and self-hosted GitLab, on
// net/http against REST v4). defaultNewClient picks between them from the
// repository's own git remote host, so a caller never names a forge.
//
// Routing happens BEFORE any credential is read. That ordering is deliberate:
// if a missing token could change the answer, a GitLab repo with no GitLab
// token would fall through to the GitHub client and be reported as a missing
// GH_TOKEN — a hint that could never have helped. Token resolution is then
// per-provider (GH_TOKEN / GITHUB_TOKEN, versus GITLAB_TOKEN or an existing
// `glab auth login`), and shared GitLab credentials are host-scoped so a
// crafted remote cannot redirect them (see hosts.go).
//
// The consumed surface is the free functions, not a method set: callers never
// hold a client. The Client interface is what the two backends implement and
// what defaultNewClient returns; it doubles as the test seam, since a func-var
// indirection over newClient injects canned data.
//
// Neither REST API exposes a single reviewDecision or statusCheckRollup field.
// GitHub reconstructs both from ListReviews and check-runs/combined-status;
// GitLab reads head_pipeline directly and takes the decision from /approvals.
// Both are opt-in per call (ListOpts.WithDecision / WithCI; ViewPR always fills
// them) so a cheap ListPRs skips the extra round-trips.
package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/zzet/gortex/internal/analysis"
)

// callTimeout bounds every network round-trip a forge call makes.
const callTimeout = 30 * time.Second

// aggregateConcurrency bounds the per-PR fan-out both backends use to
// reconstruct the review-decision / CI aggregates.
const aggregateConcurrency = 8

// maxPerPage is the largest page either forge will return in one request.
const maxPerPage = 100

// boundedCtx derives a per-call context bounded by t, falling back to
// callTimeout. Both backends bound every round-trip identically, so the rule
// lives once rather than as two byte-identical methods.
func boundedCtx(ctx context.Context, t time.Duration) (context.Context, context.CancelFunc) {
	if t <= 0 {
		t = callTimeout
	}
	return context.WithTimeout(ctx, t)
}

// synthesizePatch builds a valid unified-diff fragment for one file from a
// forge's hunk-only patch body.
//
// Neither GitHub's per-file .patch nor GitLab's .diff carries the
// `--- a/… +++ b/…` header, so one has to be synthesized before
// analysis.ParseDiffHunks can scope the hunks to a file — and so the assembled
// Raw is a diff a human or tool can actually apply. An empty patch yields an
// empty string rather than a header with no hunks.
func synthesizePatch(oldPath, newPath, patch string) string {
	if patch == "" {
		return ""
	}
	if oldPath == "" {
		oldPath = newPath
	}
	out := "--- a/" + oldPath + "\n+++ b/" + newPath + "\n" + patch
	if !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out
}

// ErrNotAuthenticated is the sentinel for "no usable credential for this
// repository's forge". Callers test it with errors.Is.
//
// Its text is deliberately provider-neutral. Each backend wraps it with the
// wording for the token IT wants, so a GitLab remote never inherits a
// "set GH_TOKEN" prefix — the misdirection that made this class of failure so
// hard to read in the first place.
var ErrNotAuthenticated = errors.New("no forge token")

// ErrRateLimited is returned when a forge answers with a rate-limit error —
// GitHub's *github.RateLimitError / *github.AbuseRateLimitError, or GitLab's
// 429 — mapped onto this sentinel (with a Retry-After hint preserved in the
// wrapped error's message) so callers can test errors.Is(err, ErrRateLimited).
//
// Provider-neutral for the same reason as ErrNotAuthenticated: each backend
// formats this sentinel into its user-visible message, so a GitHub-branded
// string made a GitLab 429 read "github rate limited".
var ErrRateLimited = errors.New("forge rate limited")

// PR is the canonical pull-request value. It is built from a go-github
// *github.PullRequest; ReviewDecision and CIRollup are reconstructed (the
// REST API has no such aggregate fields). Files is EMPTY after ListPRs —
// only ViewPR / PRFiles hydrate it.
type PR struct {
	Number         int
	Title          string
	Author         string
	BaseRef        string // PullRequest.Base.Ref
	HeadRef        string // PullRequest.Head.Ref
	IsDraft        bool
	ReviewDecision string // reconstructed from ListReviews (REST has no reviewDecision field)
	CIRollup       string // reconstructed from Checks + GetCombinedStatus; collapsed by RollupCI
	UpdatedAt      time.Time
	Mergeable      string
	URL            string
	State          string
	Files          []string // EMPTY after ListPRs; only ViewPR / PRFiles hydrate it
}

// PRDiff is the per-file diff of a pull request, each file's patch parsed
// into hunks via analysis.ParseDiffHunks.
type PRDiff struct {
	Number  int
	BaseRef string
	HeadRef string
	Files   []PRFile
	Raw     string
}

// PRFile is one changed file in a PR diff.
type PRFile struct {
	Path    string
	OldPath string
	Hunks   []analysis.DiffHunk // analysis.ParseDiffHunks on the file's .GetPatch()
	Status  string
}

// ReviewComment is the one inline-comment type. Posting maps a finding
// to a ReviewComment; StartLine carries the multi-line range start and
// Side defaults to "RIGHT" (the new side).
type ReviewComment struct {
	Path      string
	Line      int
	StartLine int
	Side      string
	Body      string
}

// ListOpts tunes ListPRs. Decision/CI reconstruction is opt-in per call
// so a cheap list skips the extra round-trips.
type ListOpts struct {
	State        string
	Limit        int
	Author       string
	WithDecision bool
	WithCI       bool
}

// Client is what a forge backend implements. ghClient (GitHub) and glClient
// (GitLab) both satisfy it, and defaultNewClient selects between them per
// repository — so this is production dispatch, not only a test hook. It also
// serves as the test seam: a func-var indirection over newClient injects canned
// data without touching any caller.
type Client interface {
	ListPRs(ctx context.Context, opts ListOpts) ([]PR, error)
	ViewPR(ctx context.Context, num int) (*PR, error)
	PRFiles(ctx context.Context, num int) ([]string, error)
	DiffPR(ctx context.Context, num int) (*PRDiff, error)
	PostReviewComments(ctx context.Context, num int, comments []ReviewComment) error
}

// newClient resolves the backend Client for repoDir. It is a package var so a
// test can swap it for one backed by an httptest server with a fixed project,
// bypassing token + git-remote resolution.
//
// It returns the Client interface, not a concrete backend: that is what makes
// the seam real. Typed to *ghClient it only looked like a seam, because no
// second backend could be returned from it.
var newClient = defaultNewClient

// defaultNewClient routes repoDir to its forge backend by remote host.
//
// Routing happens BEFORE any credential is consulted (see providerFor), so a
// GitLab repository with no GitLab token reports a missing GITLAB_TOKEN rather
// than being handed to the GitHub client and reported as a missing GH_TOKEN.
func defaultNewClient(ctx context.Context, repoDir string) (Client, error) {
	kind, _, err := providerFor(ctx, repoDir)
	if err != nil {
		// The remote is unreadable (no origin, a bare directory). Hand the
		// GitHub client the same repoDir so the caller gets ONE consistent
		// error naming the unresolvable remote, rather than a client addressing
		// a slug invented from a filesystem path.
		return newGHClient(ctx, repoDir)
	}
	switch kind {
	case ProviderGitLab:
		return newGLClient(ctx, repoDir)
	default:
		return newGHClient(ctx, repoDir)
	}
}

// ListPRs lists pull requests for the repo at repoDir. PR.Files is EMPTY
// on every returned PR — triage and conflict detection must call
// PRFiles(num) explicitly per PR. Decision/CI aggregates are filled only
// when opts requests them.
func ListPRs(ctx context.Context, repoDir string, opts ListOpts) ([]PR, error) {
	c, err := newClient(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	return c.ListPRs(ctx, opts)
}

// ViewPR fetches a single pull request and hydrates its Files plus the
// reconstructed ReviewDecision / CIRollup aggregates.
func ViewPR(ctx context.Context, repoDir string, num int) (*PR, error) {
	c, err := newClient(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	return c.ViewPR(ctx, num)
}

// PRFiles returns the paths of files changed in a pull request.
func PRFiles(ctx context.Context, repoDir string, num int) ([]string, error) {
	c, err := newClient(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	return c.PRFiles(ctx, num)
}

// DiffPR returns the per-file diff of a pull request, each file's patch
// parsed into hunks.
func DiffPR(ctx context.Context, repoDir string, num int) (*PRDiff, error) {
	c, err := newClient(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	return c.DiffPR(ctx, num)
}

// PostReviewComments posts a batch of inline review comments on a pull
// request as a single review.
func PostReviewComments(ctx context.Context, repoDir string, num int, comments []ReviewComment) error {
	c, err := newClient(ctx, repoDir)
	if err != nil {
		return err
	}
	return c.PostReviewComments(ctx, num, comments)
}

// Available reports whether a usable credential exists for the forge that
// serves repoDir — GH_TOKEN / GITHUB_TOKEN for GitHub, GITLAB_TOKEN or the
// user's glab-cli login for GitLab.
//
// It takes repoDir because "is the forge available" is not a global question:
// answering it from the GitHub token alone is what made a GitLab repository
// report `forge unavailable: set GH_TOKEN`, a hint that could never have helped.
func Available(ctx context.Context, repoDir string) bool {
	kind, r, err := providerFor(ctx, repoDir)
	if err != nil {
		return resolveToken() != ""
	}
	if kind == ProviderGitLab {
		return resolveGitLabToken(r.Host) != ""
	}
	return resolveToken() != ""
}

// MissingTokenHint returns the actionable hint for a repository whose forge has
// no usable credential, naming the variable that would actually help.
//
// It deliberately carries no "in the daemon environment" clause: the CLI
// resolves tokens from its OWN environment while the MCP tools resolve them
// from the daemon's, so the caller appends whichever is true for it.
func MissingTokenHint(ctx context.Context, repoDir string) string {
	kind, r, err := providerFor(ctx, repoDir)
	if err == nil && kind == ProviderGitLab {
		return "set GITLAB_TOKEN (or run `glab auth login --hostname " + r.Host + "`)"
	}
	return "set GH_TOKEN (or GITHUB_TOKEN)"
}

// ProviderName reports which forge backend serves repoDir, as "github" or
// "gitlab". An unresolvable remote reports "github", the historical default.
func ProviderName(ctx context.Context, repoDir string) string {
	kind, _, err := providerFor(ctx, repoDir)
	if err != nil {
		return string(ProviderGitHub)
	}
	return string(kind)
}

// PRWebURL returns the canonical browser URL for pull/merge request num on
// repoDir's forge, or "" when the remote cannot be resolved.
//
// GitHub and GitLab disagree on both the path segment and the project shape:
// /owner/repo/pull/N versus /group/subgroup/project/-/merge_requests/N. The
// "/-/" separator is what lets GitLab tell a nested group path from the route.
func PRWebURL(ctx context.Context, repoDir string, num int) string {
	kind, r, err := providerFor(ctx, repoDir)
	if err != nil || num <= 0 {
		return ""
	}
	if kind == ProviderGitLab {
		return fmt.Sprintf("https://%s/%s/-/merge_requests/%d", r.Host, r.Path, num)
	}
	return fmt.Sprintf("https://%s/%s/pull/%d", r.Host, r.Path, num)
}
