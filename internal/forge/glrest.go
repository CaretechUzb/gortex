package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/zzet/gortex/internal/analysis"
)

// glClient talks to a GitLab instance's REST v4 API for a single project.
//
// It is hand-rolled on net/http rather than pulling in a GitLab SDK: the
// consumed surface is five endpoints, and this repo already prefers stdlib for
// a bounded API surface (the Bedrock LLM provider signs SigV4 by hand rather
// than take the AWS SDK).
type glClient struct {
	http    *http.Client
	base    string // REST v4 base, trailing slash: "https://host/api/v4/"
	project string // URL-escaped full project path: "group%2Fsub%2Fproject"
	host    string
	token   string
	timeout time.Duration
}

// newGLClient resolves the token and project path for repoDir and builds a
// glClient. A missing token surfaces a host-aware wrap of ErrNotAuthenticated
// naming GITLAB_TOKEN — never GH_TOKEN.
func newGLClient(ctx context.Context, repoDir string) (*glClient, error) {
	r, err := resolveRemote(ctx, repoDir)
	if err != nil {
		return nil, err
	}
	tok := resolveGitLabToken(r.Host)
	if tok == "" {
		return nil, errNoGitLabToken(r.Host)
	}
	return &glClient{
		http:    newGLHTTPClient(),
		base:    gitlabAPIBase(r.Host),
		project: url.PathEscape(r.Path),
		host:    r.Host,
		token:   tok,
		timeout: callTimeout,
	}, nil
}

// glTransport is shared by every glClient so connections pool across the
// aggregate fan-out. DefaultTransport's MaxIdleConnsPerHost is 2, well under
// the errgroup's limit of 8, which costs a fresh TCP+TLS handshake per wave on
// an HTTP/1.1 instance.
var glTransport = sync.OnceValue(func() http.RoundTripper {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = aggregateConcurrency
	return t
})

// newGLHTTPClient builds the HTTP client used for every GitLab call.
//
// CheckRedirect is the security-relevant part: a redirect that changes origin
// is refused outright rather than followed with credentials attached. Combined
// with sending the token on Authorization (which Go itself strips cross-host),
// that is belt and braces — the header alone would already be stripped, but a
// same-host-scheme downgrade to plain http would not be.
func newGLHTTPClient() *http.Client {
	return &http.Client{
		Transport: glTransport(),
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("forge: too many redirects")
			}
			first := via[0].URL
			if !strings.EqualFold(req.URL.Host, first.Host) || !strings.EqualFold(req.URL.Scheme, first.Scheme) {
				// Do not follow; hand the caller the 30x itself.
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

// callCtx derives a per-call context bounded by the client's timeout.
func (c *glClient) callCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return boundedCtx(ctx, c.timeout)
}

// projectPath builds an API path under this client's project, e.g.
// projectPath("merge_requests/7/changes").
func (c *glClient) projectPath(suffix string) string {
	return c.base + "projects/" + c.project + "/" + suffix
}

// do issues one API request and decodes a JSON response into out (which may be
// nil to discard the body). Non-2xx responses are mapped by glMapErr.
func (c *glClient) do(ctx context.Context, method, rawURL string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("forge: encoding gitlab request: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return fmt.Errorf("forge: building gitlab request: %w", err)
	}
	// Authorization, deliberately NOT the PRIVATE-TOKEN header GitLab also
	// accepts. Go's net/http strips a fixed set of sensitive headers on a
	// cross-host redirect — Authorization among them — and copies every other
	// header verbatim. A token on PRIVATE-TOKEN therefore survives a 30x to an
	// arbitrary origin, which is a credential leak the GitHub backend never had
	// (go-github uses Authorization). GitLab accepts Bearer for personal,
	// project, and CI job tokens alike.
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("forge: gitlab request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return glMapErr(resp, payload)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("forge: decoding gitlab response: %w", err)
	}
	return nil
}

// glHTTPError carries the HTTP status of a non-2xx GitLab response alongside
// the rendered message, so a caller can branch on the STATUS CODE instead of
// grepping the message for "403" — a substring that also appears in the body of
// an unrelated 500, which would make a hard failure look like a soft degrade.
//
// Unwrap exposes the package sentinel (ErrRateLimited / ErrNotAuthenticated)
// where one applies, so errors.Is keeps working for existing callers.
type glHTTPError struct {
	Status   int
	msg      string
	sentinel error
}

func (e *glHTTPError) Error() string { return e.msg }
func (e *glHTTPError) Unwrap() error { return e.sentinel }

// glMapErr folds a non-2xx GitLab response onto this package's sentinels:
// 429 (and a 403 carrying an exhausted rate-limit quota) become ErrRateLimited
// so callers can errors.Is on it exactly as they do for GitHub; 401 becomes
// ErrNotAuthenticated. Every result carries the status via *glHTTPError.
func glMapErr(resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	wrap := func(sentinel error, format string, args ...any) error {
		return &glHTTPError{
			Status:   resp.StatusCode,
			msg:      fmt.Sprintf(format, args...),
			sentinel: sentinel,
		}
	}
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		if ra := strings.TrimSpace(resp.Header.Get("Retry-After")); ra != "" {
			return wrap(ErrRateLimited, "%s (retry after %ss)", ErrRateLimited, ra)
		}
		return wrap(ErrRateLimited, "%s: %s", ErrRateLimited, msg)
	case resp.StatusCode == http.StatusUnauthorized:
		return wrap(ErrNotAuthenticated, "%s: gitlab rejected the token (401)", ErrNotAuthenticated)
	case resp.StatusCode == http.StatusForbidden && resp.Header.Get("RateLimit-Remaining") == "0":
		return wrap(ErrRateLimited, "%s: gitlab rate limit exhausted", ErrRateLimited)
	}
	return wrap(nil, "forge: gitlab %s: %s", resp.Status, msg)
}

// glUser is the author/assignee shape GitLab embeds in an MR.
type glUser struct {
	Username string `json:"username"`
}

// glDiffRefs carries the three SHAs a positioned discussion must quote. All
// three are required by the discussions API; a comment posted without them is
// rejected or silently lands unpositioned.
type glDiffRefs struct {
	BaseSHA  string `json:"base_sha"`
	HeadSHA  string `json:"head_sha"`
	StartSHA string `json:"start_sha"`
}

// glPipeline is the MR's head pipeline. GitLab reports the CI rollup directly,
// so unlike GitHub there is nothing to reconstruct from check-runs.
type glPipeline struct {
	Status string `json:"status"`
}

// glMR is the subset of a GitLab merge request this package consumes.
//
// IID, not ID: ID is instance-global and meaningless in a project URL, while
// IID is the per-project number every UI, URL, and `glab` command uses.
type glMR struct {
	IID          int         `json:"iid"`
	Title        string      `json:"title"`
	Author       glUser      `json:"author"`
	SourceBranch string      `json:"source_branch"`
	TargetBranch string      `json:"target_branch"`
	Draft        bool        `json:"draft"`
	WorkInProg   bool        `json:"work_in_progress"`
	State        string      `json:"state"`
	WebURL       string      `json:"web_url"`
	UpdatedAt    time.Time   `json:"updated_at"`
	MergeStatus  string      `json:"merge_status"`
	DetailedMS   string      `json:"detailed_merge_status"`
	HasConflicts bool        `json:"has_conflicts"`
	DiffRefs     glDiffRefs  `json:"diff_refs"`
	HeadPipeline *glPipeline `json:"head_pipeline"`
}

// glChange is one changed file of an MR, from the /changes endpoint.
type glChange struct {
	OldPath     string `json:"old_path"`
	NewPath     string `json:"new_path"`
	NewFile     bool   `json:"new_file"`
	RenamedFile bool   `json:"renamed_file"`
	DeletedFile bool   `json:"deleted_file"`
	Diff        string `json:"diff"`
	TooLarge    bool   `json:"too_large"`
}

// glChangesResponse wraps the /changes payload.
//
// Overflow is GitLab's signal that it truncated the change set because the MR
// exceeded a size limit. Ignoring it means a huge MR silently yields a PARTIAL
// file list, which then feeds the impact score as if it were the whole change —
// a truncated blast radius reported with full confidence.
type glChangesResponse struct {
	Changes  []glChange `json:"changes"`
	Overflow bool       `json:"overflow"`
}

// glApprovals is the /approvals payload. approved is available on CE; the
// richer approval-rule fields are Premium and deliberately not read here.
//
// approved_by is an array of WRAPPERS — [{"user":{"username":…}}] — not of users.
// Decoding it as []glUser silently yields empty usernames; only the length is
// read today, so it would have failed quietly the moment anyone used a name.
type glApprovals struct {
	Approved   bool           `json:"approved"`
	ApprovedBy []glApprovedBy `json:"approved_by"`
}

// glApprovedBy is one entry of the approvals payload's approved_by array.
type glApprovedBy struct {
	User glUser `json:"user"`
}

// glListState maps this package's GitHub-shaped state vocabulary
// (open/closed/all) onto GitLab's (opened/closed/merged/all). An unrecognized
// value is passed through so a caller can name a GitLab state directly.
func glListState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "open":
		return "opened"
	case "closed":
		return "closed"
	case "all":
		return "all"
	default:
		return strings.ToLower(strings.TrimSpace(state))
	}
}

// glPipelineState normalizes a GitLab pipeline status to the
// failure / pending / success vocabulary collapseStates consumes, so the CI
// rollup lands on the same NONE / FAILURE / PENDING / SUCCESS scale as GitHub's.
func glPipelineState(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "success", "skipped":
		return "success"
	case "failed", "canceled", "cancelled":
		return "failure"
	case "":
		return ""
	default:
		// created, waiting_for_resource, preparing, pending, running, scheduled,
		// and MANUAL — a manual pipeline is BLOCKED awaiting a human-triggered
		// job, which GitLab's own merge checks treat as not-succeeded. Collapsing
		// it to success reported a gated MR as READY with ci=SUCCESS.
		return "pending"
	}
}

// glFileStatus maps GitLab's per-file boolean flags onto the GitHub-shaped
// status vocabulary PRFile.Status carries.
func glFileStatus(ch glChange) string {
	switch {
	case ch.NewFile:
		return "added"
	case ch.DeletedFile:
		return "removed"
	case ch.RenamedFile:
		return "renamed"
	default:
		return "modified"
	}
}

// prFromGL projects a GitLab merge request onto a forge.PR. It does NOT
// hydrate Files or the aggregates, mirroring prFromGH.
func prFromGL(m glMR) PR {
	mergeable := m.DetailedMS
	if mergeable == "" {
		mergeable = m.MergeStatus
	}
	if m.HasConflicts {
		mergeable = "conflicts"
	}
	return PR{
		Number:    m.IID,
		Title:     m.Title,
		Author:    m.Author.Username,
		BaseRef:   m.TargetBranch,
		HeadRef:   m.SourceBranch,
		IsDraft:   m.Draft || m.WorkInProg,
		UpdatedAt: m.UpdatedAt,
		Mergeable: mergeable,
		URL:       m.WebURL,
		State:     glPRState(m.State),
	}
}

// glPRState maps GitLab's MR state onto the GitHub-shaped vocabulary the rest
// of the package reads. GitLab's "merged" is a distinct state where GitHub
// reports "closed", so it is normalized to "closed" to keep classification and
// filtering consistent across backends.
func glPRState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "opened":
		return "open"
	case "merged", "closed", "locked":
		return "closed"
	default:
		return strings.ToLower(strings.TrimSpace(state))
	}
}

// ListPRs lists merge requests. PR.Files is left empty and the decision / CI
// aggregates are filled only when opts requests them — the same contract
// ghClient.ListPRs honours.
func (c *glClient) ListPRs(ctx context.Context, opts ListOpts) ([]PR, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()

	perPage := opts.Limit
	if perPage <= 0 || perPage > maxPerPage {
		perPage = maxPerPage
	}
	q := url.Values{}
	q.Set("state", glListState(opts.State))
	q.Set("per_page", strconv.Itoa(perPage))
	q.Set("order_by", "updated_at")
	if opts.Author != "" {
		q.Set("author_username", opts.Author)
	}

	var mrs []glMR
	if err := c.do(cctx, http.MethodGet, c.projectPath("merge_requests")+"?"+q.Encode(), nil, &mrs); err != nil {
		return nil, err
	}

	out := make([]PR, 0, len(mrs))
	for _, m := range mrs {
		out = append(out, prFromGL(m))
		if opts.Limit > 0 && len(out) >= opts.Limit {
			break
		}
	}

	if opts.WithDecision || opts.WithCI {
		if err := c.fillAggregates(cctx, out, opts.WithDecision, opts.WithCI); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// fillAggregates fills the requested ReviewDecision / CIRollup fields for every
// MR in prs, fetching concurrently under the same bounded errgroup ghClient
// uses. GitLab's list endpoint omits head_pipeline, so CI needs the single-MR
// GET; approvals are their own endpoint.
func (c *glClient) fillAggregates(ctx context.Context, prs []PR, withDecision, withCI bool) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(aggregateConcurrency)
	for i := range prs {
		i := i
		g.Go(func() error {
			if withCI {
				m, err := c.fetchMR(gctx, prs[i].Number)
				if err != nil {
					return err
				}
				prs[i].CIRollup = ciFromPipeline(m.HeadPipeline)
			}
			if withDecision {
				dec, err := c.reviewDecision(gctx, prs[i].Number)
				if err != nil {
					return err
				}
				prs[i].ReviewDecision = dec
			}
			return nil
		})
	}
	return g.Wait()
}

// ciFromPipeline collapses an MR's head pipeline onto the shared rollup scale.
// A merge request with no pipeline reads NONE, exactly as a PR with no checks.
func ciFromPipeline(p *glPipeline) string {
	if p == nil {
		return "NONE"
	}
	state := glPipelineState(p.Status)
	if state == "" {
		return "NONE"
	}
	return collapseStates([]string{state})
}

// fetchMR gets a single merge request by IID.
func (c *glClient) fetchMR(ctx context.Context, num int) (*glMR, error) {
	var m glMR
	if err := c.do(ctx, http.MethodGet, c.projectPath("merge_requests/"+strconv.Itoa(num)), nil, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// reviewDecision reconstructs an MR's review decision from its approvals.
//
// GitLab CE has no "request changes" verb, so the vocabulary is narrower than
// GitHub's: APPROVED or REVIEW_REQUIRED. A 403 (approvals gated behind a paid
// tier) degrades to an empty decision rather than failing the whole listing —
// an unavailable aggregate must not cost the caller the MRs themselves.
func (c *glClient) reviewDecision(ctx context.Context, num int) (string, error) {
	var a glApprovals
	err := c.do(ctx, http.MethodGet, c.projectPath("merge_requests/"+strconv.Itoa(num)+"/approvals"), nil, &a)
	if err != nil {
		// Branch on the STATUS, never on the message: a 500 whose body happens
		// to mention 403 must stay a hard failure, not a silent degrade.
		var he *glHTTPError
		if errors.As(err, &he) && (he.Status == http.StatusForbidden || he.Status == http.StatusNotFound) {
			return "", nil
		}
		return "", err
	}
	if a.Approved || len(a.ApprovedBy) > 0 {
		return "APPROVED", nil
	}
	return "REVIEW_REQUIRED", nil
}

// ViewPR fetches a single merge request and hydrates Files plus the
// ReviewDecision / CIRollup aggregates.
func (c *glClient) ViewPR(ctx context.Context, num int) (*PR, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()

	m, err := c.fetchMR(cctx, num)
	if err != nil {
		return nil, err
	}
	pr := prFromGL(*m)

	changes, err := c.listChanges(cctx, num)
	if err != nil {
		return nil, err
	}
	pr.Files = make([]string, 0, len(changes))
	for _, ch := range changes {
		pr.Files = append(pr.Files, changedPath(ch))
	}

	dec, err := c.reviewDecision(cctx, num)
	if err != nil {
		return nil, err
	}
	pr.ReviewDecision = dec
	pr.CIRollup = ciFromPipeline(m.HeadPipeline)
	return &pr, nil
}

// changedPath returns the path a change should be reported under: the new path
// normally, the old path for a deletion (where new_path repeats the old one but
// the file no longer exists at head).
func changedPath(ch glChange) string {
	if ch.DeletedFile && ch.OldPath != "" {
		return ch.OldPath
	}
	if ch.NewPath != "" {
		return ch.NewPath
	}
	return ch.OldPath
}

// PRFiles returns the changed file paths of a merge request.
func (c *glClient) PRFiles(ctx context.Context, num int) ([]string, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()

	changes, err := c.listChanges(cctx, num)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(changes))
	for _, ch := range changes {
		out = append(out, changedPath(ch))
	}
	return out, nil
}

// listChanges fetches the changed-file records of a merge request.
//
// A truncated set is an ERROR, not a short answer: every caller (PRFiles,
// ViewPR, DiffPR) feeds it into impact scoring or review anchoring, where a
// silently partial file list is worse than no answer at all.
func (c *glClient) listChanges(ctx context.Context, num int) ([]glChange, error) {
	var resp glChangesResponse
	if err := c.do(ctx, http.MethodGet, c.projectPath("merge_requests/"+strconv.Itoa(num)+"/changes"), nil, &resp); err != nil {
		return nil, err
	}
	if resp.Overflow {
		return nil, fmt.Errorf("forge: gitlab MR !%d is too large — GitLab truncated its change set (%d files returned); "+
			"the file list would be incomplete", num, len(resp.Changes))
	}
	return resp.Changes, nil
}

// DiffPR returns the per-file diff, each file's patch run through
// analysis.ParseDiffHunks.
func (c *glClient) DiffPR(ctx context.Context, num int) (*PRDiff, error) {
	cctx, cancel := c.callCtx(ctx)
	defer cancel()

	m, err := c.fetchMR(cctx, num)
	if err != nil {
		return nil, err
	}
	changes, err := c.listChanges(cctx, num)
	if err != nil {
		return nil, err
	}

	diff := &PRDiff{
		Number:  m.IID,
		BaseRef: m.TargetBranch,
		HeadRef: m.SourceBranch,
	}
	var raw strings.Builder
	for _, ch := range changes {
		path := changedPath(ch)
		// GitLab's per-file .diff carries only the hunk body — no
		// `+++ b/<file>` header — so synthesize one exactly as the GitHub
		// backend does, both to scope ParseDiffHunks to this file and to keep
		// Raw a valid unified diff.
		//
		// A too_large / elided diff yields no hunks rather than a bogus empty
		// one: the file still appears in Files, it simply carries no anchors.
		var withHeader string
		if ch.Diff != "" && !ch.TooLarge {
			withHeader = synthesizePatch(oldPathOf(ch, path), path, ch.Diff)
		}
		diff.Files = append(diff.Files, PRFile{
			Path:    path,
			OldPath: renamedFrom(ch),
			Status:  glFileStatus(ch),
			Hunks:   analysis.ParseDiffHunks(withHeader),
		})
		raw.WriteString(withHeader)
	}
	diff.Raw = raw.String()
	return diff, nil
}

// oldPathOf returns the path to name on the `--- a/` side of a synthesized
// diff header.
func oldPathOf(ch glChange, newPath string) string {
	if ch.OldPath != "" {
		return ch.OldPath
	}
	return newPath
}

// renamedFrom reports the previous path only for an actual rename, matching
// GitHub's GetPreviousFilename (empty for every non-rename).
func renamedFrom(ch glChange) string {
	if ch.RenamedFile {
		return ch.OldPath
	}
	return ""
}

// glDiscussion is the POST body for one positioned discussion.
type glDiscussion struct {
	Body     string      `json:"body"`
	Position *glPosition `json:"position,omitempty"`
}

// glPosition anchors a discussion to a line of the MR diff. All three SHAs are
// mandatory; position_type is "text" for a line comment.
type glPosition struct {
	BaseSHA      string `json:"base_sha"`
	HeadSHA      string `json:"head_sha"`
	StartSHA     string `json:"start_sha"`
	PositionType string `json:"position_type"`
	NewPath      string `json:"new_path"`
	OldPath      string `json:"old_path"`
	NewLine      int    `json:"new_line,omitempty"`
	OldLine      int    `json:"old_line,omitempty"`
}

// PartialPostError reports how many inline comments actually landed before a
// non-atomic post failed.
//
// The count has to travel as DATA, not inside a message string: the caller
// reports it to the user, and a per-comment backend that failed at comment 1 of
// 40 must not be reported as 40 posted (nor as 0, which would invite a retry
// that double-posts the ones that did land).
type PartialPostError struct {
	Posted int
	Total  int
	Err    error
}

func (e *PartialPostError) Error() string {
	return fmt.Sprintf("forge: posted %d of %d comments, then: %v", e.Posted, e.Total, e.Err)
}
func (e *PartialPostError) Unwrap() error { return e.Err }

// PostReviewComments posts each inline comment as its own positioned
// discussion.
//
// This is the one place the two backends genuinely diverge. GitHub batches every
// comment into a single review in one request; GitLab has no review-batch
// concept, so N comments are N POSTs and the operation is NOT atomic. A failure
// partway through leaves the earlier discussions on the merge request, so the
// error names how many landed — a caller that retried blindly would double-post
// them.
func (c *glClient) PostReviewComments(ctx context.Context, num int, comments []ReviewComment) error {
	if len(comments) == 0 {
		return nil
	}
	// Each request gets its OWN budget. One shared 30s deadline across 1+N
	// sequential POSTs made partial posts routine rather than exceptional: at a
	// realistic 250-400ms per discussion, a 40-finding review exhausted it
	// mid-loop, leaving half the comments on the merge request and no safe
	// retry. The GitHub backend spends the same 30s on a single request.
	mctx, mcancel := c.callCtx(ctx)
	m, err := c.fetchMR(mctx, num)
	mcancel()
	if err != nil {
		return &PartialPostError{Posted: 0, Total: len(comments), Err: err}
	}
	refs := m.DiffRefs
	if refs.BaseSHA == "" || refs.HeadSHA == "" || refs.StartSHA == "" {
		return &PartialPostError{
			Posted: 0, Total: len(comments),
			Err: fmt.Errorf("gitlab MR !%d has no diff refs; cannot anchor inline comments", num),
		}
	}

	endpoint := c.projectPath("merge_requests/" + strconv.Itoa(num) + "/discussions")
	for i, rc := range comments {
		body := glDiscussion{
			Body:     rc.Body,
			Position: positionFor(rc, refs),
		}
		pctx, pcancel := c.callCtx(ctx)
		err := c.do(pctx, http.MethodPost, endpoint, body, nil)
		pcancel()
		if err != nil {
			return &PartialPostError{Posted: i, Total: len(comments), Err: err}
		}
	}
	return nil
}

// positionFor builds the position object for one comment. Side "LEFT" anchors
// to the old file at old_line; anything else anchors to the new side, which is
// where review findings live.
func positionFor(rc ReviewComment, refs glDiffRefs) *glPosition {
	p := &glPosition{
		BaseSHA:      refs.BaseSHA,
		HeadSHA:      refs.HeadSHA,
		StartSHA:     refs.StartSHA,
		PositionType: "text",
		NewPath:      rc.Path,
		OldPath:      rc.Path,
	}
	if strings.EqualFold(rc.Side, "LEFT") {
		p.OldLine = rc.Line
		return p
	}
	p.NewLine = rc.Line
	return p
}
