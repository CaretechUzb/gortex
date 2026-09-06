package indexer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/zzet/gortex/internal/gitcmd"
	"github.com/zzet/gortex/internal/pathkey"
)

// This file is the one place the indexer is allowed to ask "what commit is
// this checkout at?".
//
// `git -C <dir> rev-parse HEAD` is not scoped to a repository, it is scoped to
// a directory: git walks UP from that directory until it finds a repository.
// A checkout whose `.git` link has been unlinked is still a directory, so the
// walk keeps going into whatever encloses it -- and every `git worktree
// remove` and every `rm -rf <checkout>` passes through that window, because
// neither removal is atomic. For a worktree of a submodule living inside its
// superproject the enclosing repository is the superproject, and rev-parse
// answers with the SUPERPROJECT's HEAD without erroring at all.
//
// Observed 2026-09-04: the ref watcher for `local@MR6410` read
// 038bba933ed0 -- the superproject's HEAD, not an object in `local` -- diffed
// it against `local`'s own 5a206c32f778 (which resolved only because the two
// repositories share history), and deleted 2,845 files from the checkout's
// graph. Four call sites shell out for a HEAD this way; every one of them
// could have been the first to read the wrong repository, so the guard lives
// with the resolution rather than at any single caller.

// The fail-closed refusals a HEAD resolution can take before it returns a
// commit. They are distinct because they mean different things to whoever
// reads the log: the first two say the checkout is on its way out and belongs
// to the checkout-lifecycle sweep, the third says the checkout is still here
// but git answered for somebody else's repository, and the fourth says git
// itself answered in a shape this code does not understand -- a tooling
// problem, not a checkout problem, and it must not be reported as one.
var (
	errCheckoutRootGone        = errors.New("checkout root is gone")
	errCheckoutGitLinkGone     = errors.New("checkout .git link is gone")
	errForeignGitIdentity      = errors.New("HEAD resolved through a foreign repository")
	errHeadIdentityUnparseable = errors.New("git returned an unparseable HEAD identity")
)

// gitIdentityRefusal names the refusal an error carries, or "" when the error
// is an ordinary git failure (an unborn HEAD, a busy index) that callers have
// always swallowed silently.
func gitIdentityRefusal(err error) string {
	switch {
	case errors.Is(err, errCheckoutRootGone):
		return "root_gone"
	case errors.Is(err, errCheckoutGitLinkGone):
		return "git_link_gone"
	case errors.Is(err, errForeignGitIdentity):
		return "foreign_repository"
	case errors.Is(err, errHeadIdentityUnparseable):
		return "unparseable_git_output"
	default:
		return ""
	}
}

// checkoutHeadSHA resolves HEAD for checkoutRoot and proves the answer came
// from THAT checkout's own repository before returning it.
//
// expectedCommonDir is the family the caller already knows this checkout
// belongs to -- a Git watcher carries the one it was admitted under, which is
// the strongest baseline available because it also catches a `.git` link
// rewritten to point elsewhere after admission. A caller with no such record
// passes "" and the baseline is resolved from the checkout's own `.git` link
// instead: pure path work (read the link, read its commondir file) that,
// unlike rev-parse, cannot climb out of the checkout.
//
// The guard is identity, not liveness. A root or `.git` link that is already
// gone is refused outright and left to the checkout-lifecycle sweep, which is
// the only path allowed to retire a vanished checkout: no freshness stamp, no
// ref reconcile, and no copy-source decision may be the thing that acts on a
// checkout that is disappearing.
func checkoutHeadSHA(ctx context.Context, checkoutRoot, expectedCommonDir string) (string, error) {
	if checkoutRoot == "" {
		return "", fmt.Errorf("%w: empty checkout root", errCheckoutRootGone)
	}
	if WorktreeRootGone(checkoutRoot) {
		return "", fmt.Errorf("%w: %s", errCheckoutRootGone, checkoutRoot)
	}
	// Fail closed on every stat error, not only ErrNotExist. Refusing costs the
	// caller one skipped observation that it already knows how to retry;
	// guessing costs a whole-checkout eviction or a freshness row stamped at
	// another repository's commit.
	if _, err := os.Lstat(filepath.Join(checkoutRoot, ".git")); err != nil {
		return "", fmt.Errorf("%w: %w", errCheckoutGitLinkGone, err)
	}

	expected := expectedCommonDir
	if expected == "" {
		gitDir, err := resolveGitDir(checkoutRoot)
		if err != nil {
			return "", fmt.Errorf("%w: %w", errCheckoutGitLinkGone, err)
		}
		expected = filepath.Clean(gitCommonDir(checkoutRoot, gitDir))
	}

	common, head, err := resolveHeadIdentity(ctx, checkoutRoot)
	if err != nil {
		return "", err
	}
	if !sameGitPath(common, expected) {
		return "", fmt.Errorf("%w: resolved common dir %s, expected %s (head %s)",
			errForeignGitIdentity, common, expected, head)
	}
	return head, nil
}

// git's `--path-format` capability, latched process-wide after the first
// answer. It is a property of the git BINARY, not of any repository, so one
// observation settles it for every later call.
//
// Without the latch every HEAD read on a git too old for --path-format pays
// two subprocesses at four call sites, and -- worse -- so did every ordinary
// failure (an unborn HEAD, a root that is not a repository), because the retry
// fired on any error at all rather than on the one error that the retry can
// actually fix.
const (
	pathFormatUnknown int32 = 0
	pathFormatYes     int32 = 1
	pathFormatNo      int32 = -1
)

var gitPathFormatCapability atomic.Int32

// pathFormatUnsupported reports whether err is git refusing the OPTION, which
// is the only failure a retry without it can fix. gitcmd folds git's stderr
// into the error, so the refusal text is available here; every other failure
// (not a repository, unborn HEAD, cancelled context) would fail identically on
// the retry and is returned as-is.
func pathFormatUnsupported(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unknown option") ||
		strings.Contains(msg, "unknown argument") ||
		strings.Contains(msg, "usage: git rev-parse")
}

// resolveHeadIdentity reads the common directory and HEAD out of ONE rev-parse.
// Two calls would let the checkout change shape between them, which is exactly
// the race being guarded against: the verified repository has to be the
// repository the commit was read from.
func resolveHeadIdentity(ctx context.Context, checkoutRoot string) (string, string, error) {
	if gitPathFormatCapability.Load() != pathFormatNo {
		out, err := gitcmd.Output(ctx, checkoutRoot,
			"rev-parse", "--path-format=absolute", "--git-common-dir", "HEAD")
		if err == nil {
			gitPathFormatCapability.CompareAndSwap(pathFormatUnknown, pathFormatYes)
			return parseHeadIdentity(out, checkoutRoot)
		}
		if !pathFormatUnsupported(err) {
			return "", "", err
		}
		gitPathFormatCapability.Store(pathFormatNo)
	}
	// Older git releases reject --path-format and print the common directory
	// relative to the checkout; parseHeadIdentity resolves it against the root.
	out, err := gitcmd.Output(ctx, checkoutRoot, "rev-parse", "--git-common-dir", "HEAD")
	if err != nil {
		return "", "", err
	}
	return parseHeadIdentity(out, checkoutRoot)
}

// parseHeadIdentity decodes the two-line rev-parse answer. A shape this does
// not understand is errHeadIdentityUnparseable, never errForeignGitIdentity:
// git printing something unexpected says nothing about which repository
// answered, and reporting it as "this checkout resolves to a foreign
// repository" would send whoever reads the log after the wrong thing.
func parseHeadIdentity(out, checkoutRoot string) (string, string, error) {
	// Split on newlines rather than fields: a git common directory may contain
	// spaces.
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		return "", "", fmt.Errorf("%w: rev-parse returned %d lines, want 2", errHeadIdentityUnparseable, len(lines))
	}
	common := strings.TrimSpace(lines[0])
	head := strings.TrimSpace(lines[1])
	if common == "" || head == "" {
		return "", "", fmt.Errorf("%w: rev-parse returned an empty field", errHeadIdentityUnparseable)
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(checkoutRoot, common)
	}
	return filepath.Clean(common), head, nil
}

// sameGitPath reports whether two git administrative paths name the same
// directory.
//
// Both sides are canonicalised the way the view layer canonicalises a worktree
// selector root (mcp.canonicalWorktreeSelectorRoot): EvalSymlinks when it
// answers, the lexically cleaned path when it does not. The symlink step is
// load-bearing rather than defensive — git reports the physical path getcwd()
// hands it, while a caller's baseline can carry a symlinked one
// (/tmp -> /private/tmp on Darwin, and every t.TempDir() under it).
//
// The comparison itself is pathkey.EqualPaths, not string equality, so the two
// spellings a case-insensitive or Unicode-normalising filesystem can hand back
// for one directory compare equal: macOS returns decomposed (NFD) names for
// paths created with composed ones, and both macOS and Windows fold case.
// String equality read those as two different repositories and refused a
// checkout that was its own — a fail-closed guard's worst failure mode is
// refusing the honest case, because nothing downstream retries it.
//
// Falling back to the lexical path rather than refusing an unresolvable one
// cannot admit a foreign repository: the fold is a normalisation, so a lexical
// comparison is exactly the fast path this function already accepted before it
// ever called EvalSymlinks.
func sameGitPath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return pathkey.EqualPaths(canonicalGitPath(a), canonicalGitPath(b))
}

// canonicalGitPath resolves aliases where it can and keeps the lexical path
// where it cannot. Mirrors mcp.canonicalWorktreeSelectorRoot deliberately: a
// checkout root has to canonicalise identically on both sides of the daemon,
// or the view layer and this guard would disagree about which checkout a
// request names.
func canonicalGitPath(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}
