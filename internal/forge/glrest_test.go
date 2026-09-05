package forge

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Recorded GitLab REST v4 payloads, trimmed to the fields this package reads.
// Shapes are taken from a live self-hosted instance (GitLab CE).
const (
	glMRListJSON = `[{
		"id": 99001, "iid": 6413,
		"title": "DEV-1394 fix: speed up visit confirmation table load",
		"author": {"username": "b.nazarov"},
		"source_branch": "fix/visit-confirmation-slow-table",
		"target_branch": "16.0",
		"draft": false, "work_in_progress": false,
		"state": "opened",
		"web_url": "https://gitlab.example.com/his/his/-/merge_requests/6413",
		"updated_at": "2026-09-04T10:00:00.000Z",
		"merge_status": "can_be_merged",
		"detailed_merge_status": "mergeable",
		"has_conflicts": false
	}]`

	glMRGetJSON = `{
		"id": 99001, "iid": 6413,
		"title": "DEV-1394 fix: speed up visit confirmation table load",
		"author": {"username": "b.nazarov"},
		"source_branch": "fix/visit-confirmation-slow-table",
		"target_branch": "16.0",
		"draft": false, "work_in_progress": false,
		"state": "opened",
		"web_url": "https://gitlab.example.com/his/his/-/merge_requests/6413",
		"updated_at": "2026-09-04T10:00:00.000Z",
		"merge_status": "can_be_merged",
		"detailed_merge_status": "mergeable",
		"has_conflicts": false,
		"diff_refs": {
			"base_sha": "3b4020a1", "head_sha": "7a9db3e1", "start_sha": "f652f06d"
		},
		"head_pipeline": {"id": 17622, "status": "success", "ref": "refs/merge-requests/6413/head"}
	}`

	glChangesJSON = `{"changes": [
		{"old_path": "a/models/patient_visit.py", "new_path": "a/models/patient_visit.py",
		 "new_file": false, "renamed_file": false, "deleted_file": false, "too_large": false,
		 "diff": "@@ -1,3 +1,4 @@\n ctx\n-old\n+new\n+extra\n"},
		{"old_path": "old/name.js", "new_path": "new/name.js",
		 "new_file": false, "renamed_file": true, "deleted_file": false, "too_large": false,
		 "diff": "@@ -1,2 +1,2 @@\n-a\n+b\n"},
		{"old_path": "gone.py", "new_path": "gone-renamed-on-delete.py",
		 "new_file": false, "renamed_file": false, "deleted_file": true, "too_large": false,
		 "diff": "@@ -1,2 +0,0 @@\n-a\n-b\n"}
	]}`

	glApprovalsJSON = `{"user_has_approved": false, "user_can_approve": true, "approved": true,
		"approved_by": [{"user": {"username": "reviewer"}}]}`
)

// newTestGLClient builds a glClient pointed at a test server, bypassing token
// and git-remote resolution — the glrest analogue of newTestClient.
func newTestGLClient(t *testing.T, baseURL string) *glClient {
	t.Helper()
	return &glClient{
		http:    &http.Client{},
		base:    strings.TrimSuffix(baseURL, "/") + "/",
		project: "his%2Fhis",
		host:    "gitlab.example.com",
		token:   "test-token",
		timeout: 5 * time.Second,
	}
}

// glFixtureServer routes the GitLab endpoints this package calls. It also
// asserts the auth header on every request and records posted discussions.
func glFixtureServer(t *testing.T, posted *[]map[string]any) *httptest.Server {
	t.Helper()
	const proj = "/projects/his%2Fhis"
	mux := http.NewServeMux()

	guard := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			h(w, r)
		}
	}

	mux.HandleFunc(proj+"/merge_requests", guard(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != "opened" {
			t.Errorf("list state = %q, want opened", got)
		}
		_, _ = w.Write([]byte(glMRListJSON))
	}))
	mux.HandleFunc(proj+"/merge_requests/6413", guard(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(glMRGetJSON))
	}))
	mux.HandleFunc(proj+"/merge_requests/6413/changes", guard(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(glChangesJSON))
	}))
	mux.HandleFunc(proj+"/merge_requests/6413/approvals", guard(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(glApprovalsJSON))
	}))
	mux.HandleFunc(proj+"/merge_requests/6413/discussions", guard(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		if posted != nil {
			*posted = append(*posted, m)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"disc-1"}`))
	}))

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// TestGLListPRs_UsesIIDNotID is the guard for the single easiest bug to ship
// here: GitLab's `id` is instance-global and addresses nothing in a project
// route, while `iid` is the number in every URL and UI.
func TestGLListPRs_UsesIIDNotID(t *testing.T) {
	srv := glFixtureServer(t, nil)
	c := newTestGLClient(t, srv.URL)

	prs, err := c.ListPRs(context.Background(), ListOpts{State: "open"})
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("got %d MRs, want 1", len(prs))
	}
	p := prs[0]
	if p.Number != 6413 {
		t.Errorf("Number = %d, want the iid 6413 (not the id 99001)", p.Number)
	}
	if p.Author != "b.nazarov" {
		t.Errorf("Author = %q", p.Author)
	}
	if p.BaseRef != "16.0" || p.HeadRef != "fix/visit-confirmation-slow-table" {
		t.Errorf("Base/Head = %q/%q", p.BaseRef, p.HeadRef)
	}
	if p.State != "open" {
		t.Errorf("State = %q, want the normalized \"open\"", p.State)
	}
	if p.Mergeable != "mergeable" {
		t.Errorf("Mergeable = %q", p.Mergeable)
	}
	if len(p.Files) != 0 {
		t.Errorf("Files must be EMPTY after ListPRs, got %v", p.Files)
	}
	if p.ReviewDecision != "" || p.CIRollup != "" {
		t.Errorf("aggregates filled without opt-in: decision=%q ci=%q", p.ReviewDecision, p.CIRollup)
	}
}

func TestGLListPRs_WithDecisionAndCI(t *testing.T) {
	srv := glFixtureServer(t, nil)
	c := newTestGLClient(t, srv.URL)

	prs, err := c.ListPRs(context.Background(), ListOpts{State: "open", WithDecision: true, WithCI: true})
	if err != nil {
		t.Fatalf("ListPRs: %v", err)
	}
	if prs[0].CIRollup != "SUCCESS" {
		t.Errorf("CIRollup = %q, want SUCCESS from head_pipeline.status", prs[0].CIRollup)
	}
	if prs[0].ReviewDecision != "APPROVED" {
		t.Errorf("ReviewDecision = %q, want APPROVED", prs[0].ReviewDecision)
	}
}

func TestGLViewPR_HydratesFilesAndAggregates(t *testing.T) {
	srv := glFixtureServer(t, nil)
	c := newTestGLClient(t, srv.URL)

	pr, err := c.ViewPR(context.Background(), 6413)
	if err != nil {
		t.Fatalf("ViewPR: %v", err)
	}
	if pr.Number != 6413 {
		t.Errorf("Number = %d", pr.Number)
	}
	want := []string{"a/models/patient_visit.py", "new/name.js", "gone.py"}
	if len(pr.Files) != len(want) {
		t.Fatalf("Files = %v, want %v", pr.Files, want)
	}
	for i := range want {
		if pr.Files[i] != want[i] {
			t.Errorf("Files[%d] = %q, want %q", i, pr.Files[i], want[i])
		}
	}
	if pr.CIRollup != "SUCCESS" || pr.ReviewDecision != "APPROVED" {
		t.Errorf("aggregates: ci=%q decision=%q", pr.CIRollup, pr.ReviewDecision)
	}
}

func TestGLDiffPR_ParsesHunksAndStatuses(t *testing.T) {
	srv := glFixtureServer(t, nil)
	c := newTestGLClient(t, srv.URL)

	diff, err := c.DiffPR(context.Background(), 6413)
	if err != nil {
		t.Fatalf("DiffPR: %v", err)
	}
	if diff.Number != 6413 || diff.BaseRef != "16.0" {
		t.Errorf("Number/BaseRef = %d/%q", diff.Number, diff.BaseRef)
	}
	if len(diff.Files) != 3 {
		t.Fatalf("got %d files, want 3", len(diff.Files))
	}
	if diff.Files[0].Status != "modified" {
		t.Errorf("Files[0].Status = %q", diff.Files[0].Status)
	}
	if len(diff.Files[0].Hunks) == 0 {
		t.Errorf("Files[0] parsed no hunks; the synthesized +++ header is missing")
	}
	if diff.Files[1].Status != "renamed" || diff.Files[1].OldPath != "old/name.js" {
		t.Errorf("rename: status=%q oldpath=%q", diff.Files[1].Status, diff.Files[1].OldPath)
	}
	// A non-rename must carry no OldPath, matching GitHub's GetPreviousFilename.
	if diff.Files[0].OldPath != "" {
		t.Errorf("non-rename OldPath = %q, want empty", diff.Files[0].OldPath)
	}
	if diff.Files[2].Status != "removed" {
		t.Errorf("delete status = %q", diff.Files[2].Status)
	}
	if !strings.Contains(diff.Raw, "+++ b/a/models/patient_visit.py") {
		t.Errorf("Raw is not a valid unified diff:\n%s", diff.Raw)
	}
}

// TestGLPostReviewComments_PositionsEveryComment checks the shape that made
// GitLab posting different in the first place: N discussions, each carrying all
// three diff-ref SHAs, rather than one batched review.
func TestGLPostReviewComments_PositionsEveryComment(t *testing.T) {
	var posted []map[string]any
	srv := glFixtureServer(t, &posted)
	c := newTestGLClient(t, srv.URL)

	comments := []ReviewComment{
		{Path: "a/models/patient_visit.py", Line: 12, Side: "RIGHT", Body: "first"},
		{Path: "new/name.js", Line: 3, Side: "LEFT", Body: "second"},
	}
	if err := c.PostReviewComments(context.Background(), 6413, comments); err != nil {
		t.Fatalf("PostReviewComments: %v", err)
	}
	if len(posted) != 2 {
		t.Fatalf("posted %d discussions, want 2 (one per comment)", len(posted))
	}

	first, _ := posted[0]["position"].(map[string]any)
	if first == nil {
		t.Fatalf("first discussion carried no position")
	}
	for _, k := range []string{"base_sha", "head_sha", "start_sha"} {
		if first[k] == "" || first[k] == nil {
			t.Errorf("position is missing %s; GitLab rejects an unanchored comment", k)
		}
	}
	if first["new_line"] != float64(12) {
		t.Errorf("RIGHT side new_line = %v, want 12", first["new_line"])
	}
	if _, ok := first["old_line"]; ok {
		t.Errorf("RIGHT side must not set old_line")
	}

	second, _ := posted[1]["position"].(map[string]any)
	if second["old_line"] != float64(3) {
		t.Errorf("LEFT side old_line = %v, want 3", second["old_line"])
	}
}

// TestGLPostReviewComments_PartialFailureIsReported guards the non-atomic
// posting path: when the second of three comments fails, the error must say how
// many landed, because a blind retry would duplicate them.
func TestGLPostReviewComments_PartialFailureIsReported(t *testing.T) {
	const proj = "/projects/his%2Fhis"
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc(proj+"/merge_requests/6413", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(glMRGetJSON))
	})
	mux.HandleFunc(proj+"/merge_requests/6413/discussions", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 2 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"message":"line does not exist in the diff"}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"disc"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := newTestGLClient(t, srv.URL)
	err := c.PostReviewComments(context.Background(), 6413, []ReviewComment{
		{Path: "a.py", Line: 1, Body: "one"},
		{Path: "b.py", Line: 2, Body: "two"},
		{Path: "c.py", Line: 3, Body: "three"},
	})
	if err == nil {
		t.Fatalf("want an error when a discussion POST fails")
	}
	if !strings.Contains(err.Error(), "posted 1 of 3") {
		t.Errorf("error %q does not report how many comments landed", err)
	}
}

func TestGLMapErr_RateLimitAndAuth(t *testing.T) {
	const proj = "/projects/his%2Fhis"
	for _, tc := range []struct {
		name   string
		status int
		header map[string]string
		want   error
	}{
		{"429 is rate limited", http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, ErrRateLimited},
		{"401 is not authenticated", http.StatusUnauthorized, nil, ErrNotAuthenticated},
		{"403 with no remaining quota is rate limited", http.StatusForbidden, map[string]string{"RateLimit-Remaining": "0"}, ErrRateLimited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc(proj+"/merge_requests", func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.header {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"message":"nope"}`))
			})
			srv := httptest.NewServer(mux)
			t.Cleanup(srv.Close)

			c := newTestGLClient(t, srv.URL)
			_, err := c.ListPRs(context.Background(), ListOpts{})
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want errors.Is %v", err, tc.want)
			}
		})
	}
}

func TestGLStateAndPipelineMapping(t *testing.T) {
	for in, want := range map[string]string{
		"": "opened", "open": "opened", "closed": "closed", "all": "all", "merged": "merged",
	} {
		if got := glListState(in); got != want {
			t.Errorf("glListState(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"success": "success", "skipped": "success",
		"failed": "failure", "canceled": "failure",
		"running": "pending", "pending": "pending", "created": "pending", "scheduled": "pending",
		// manual = BLOCKED on a human-triggered job, not succeeded.
		"manual": "pending", "waiting_for_resource": "pending",
		"": "",
	} {
		if got := glPipelineState(in); got != want {
			t.Errorf("glPipelineState(%q) = %q, want %q", in, got, want)
		}
	}
	if got := ciFromPipeline(nil); got != "NONE" {
		t.Errorf("ciFromPipeline(nil) = %q, want NONE", got)
	}
}

// TestGLDraftClassifies confirms a GitLab draft MR flows through the shared,
// forge-agnostic ClassifyStatus and lands on DRAFT.
func TestGLDraftClassifies(t *testing.T) {
	pr := prFromGL(glMR{IID: 1, Draft: true, TargetBranch: "main", UpdatedAt: time.Now()})
	if st := ClassifyStatus(pr, "main"); st.State != "DRAFT" {
		t.Errorf("State = %q, want DRAFT", st.State)
	}
	// work_in_progress is the pre-14.x spelling and must classify identically.
	pr2 := prFromGL(glMR{IID: 2, WorkInProg: true, TargetBranch: "main", UpdatedAt: time.Now()})
	if st := ClassifyStatus(pr2, "main"); st.State != "DRAFT" {
		t.Errorf("work_in_progress State = %q, want DRAFT", st.State)
	}
}

// TestGLPRFiles_ReturnsChangedPaths covers glClient.PRFiles, which sat at 0%
// despite being one of the five Client interface methods.
func TestGLPRFiles_ReturnsChangedPaths(t *testing.T) {
	srv := glFixtureServer(t, nil)
	c := newTestGLClient(t, srv.URL)

	files, err := c.PRFiles(context.Background(), 6413)
	if err != nil {
		t.Fatalf("PRFiles: %v", err)
	}
	want := []string{"a/models/patient_visit.py", "new/name.js", "gone.py"}
	if len(files) != len(want) {
		t.Fatalf("PRFiles = %v, want %v", files, want)
	}
	for i := range want {
		if files[i] != want[i] {
			t.Errorf("PRFiles[%d] = %q, want %q", i, files[i], want[i])
		}
	}
}

// TestChangedPath_DeletionUsesOldPath makes the deleted-file branch
// DISCRIMINATING. The original fixture had old_path == new_path, so the test
// passed even with the branch deleted — coverage without verification.
func TestChangedPath_DeletionUsesOldPath(t *testing.T) {
	del := glChange{OldPath: "gone.py", NewPath: "gone-renamed-on-delete.py", DeletedFile: true}
	if got := changedPath(del); got != "gone.py" {
		t.Errorf("deleted file changedPath = %q, want the old path gone.py", got)
	}
	mod := glChange{OldPath: "a.py", NewPath: "a.py"}
	if got := changedPath(mod); got != "a.py" {
		t.Errorf("modified changedPath = %q", got)
	}
	ren := glChange{OldPath: "old.js", NewPath: "new.js", RenamedFile: true}
	if got := changedPath(ren); got != "new.js" {
		t.Errorf("renamed changedPath = %q, want the new path", got)
	}
	// A deletion with no old_path falls back to new_path rather than "".
	if got := changedPath(glChange{NewPath: "x.py", DeletedFile: true}); got != "x.py" {
		t.Errorf("deleted with empty old_path = %q, want x.py", got)
	}
}

// TestGLApprovals_DecodesWrappedUsers guards the payload shape. GitLab returns
// approved_by as [{"user":{"username":…}}] — an array of WRAPPERS. Decoding it
// as a flat []glUser silently yields empty usernames; only len() was read, so
// it would have failed quietly the first time anyone used a name.
func TestGLApprovals_DecodesWrappedUsers(t *testing.T) {
	var a glApprovals
	if err := json.Unmarshal([]byte(glApprovalsJSON), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !a.Approved {
		t.Error("approved = false")
	}
	if len(a.ApprovedBy) != 1 {
		t.Fatalf("approved_by len = %d, want 1", len(a.ApprovedBy))
	}
	if got := a.ApprovedBy[0].User.Username; got != "reviewer" {
		t.Errorf("approved_by[0].user.username = %q, want reviewer", got)
	}
}

// TestGLReviewDecision_DegradesOnStatusNotSubstring is the regression guard for
// a fragile check: the 403/404 soft-degrade used strings.Contains(err,"403"),
// so a 500 whose BODY merely mentioned 403 silently degraded to "no decision"
// instead of surfacing as a hard failure.
func TestGLReviewDecision_DegradesOnStatusNotSubstring(t *testing.T) {
	const proj = "/projects/his%2Fhis"
	newSrv := func(status int, body string) *glClient {
		mux := http.NewServeMux()
		mux.HandleFunc(proj+"/merge_requests/6413/approvals", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(body))
		})
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return newTestGLClient(t, srv.URL)
	}

	// A real 403 (approvals gated behind a paid tier) degrades to no decision.
	if dec, err := newSrv(http.StatusForbidden, `{"message":"403 Forbidden"}`).
		reviewDecision(context.Background(), 6413); err != nil || dec != "" {
		t.Errorf("403: dec=%q err=%v, want empty decision and no error", dec, err)
	}
	// A 500 that merely MENTIONS 403 in its body must stay a hard failure.
	if dec, err := newSrv(http.StatusInternalServerError, `{"message":"upstream said 403 while proxying"}`).
		reviewDecision(context.Background(), 6413); err == nil {
		t.Errorf("500 with \"403\" in the body degraded to dec=%q instead of erroring", dec)
	}
}

// TestGLReviewDecision_UnapprovedIsReviewRequired covers the branch every
// un-approved MR takes — the common case, and the one that drives the review
// queue. The only approvals fixture had "approved": true, so inverting the
// condition would have passed every test.
func TestGLReviewDecision_UnapprovedIsReviewRequired(t *testing.T) {
	const proj = "/projects/his%2Fhis"
	mux := http.NewServeMux()
	mux.HandleFunc(proj+"/merge_requests/6413/approvals", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"approved":false,"approved_by":[]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	dec, err := newTestGLClient(t, srv.URL).reviewDecision(context.Background(), 6413)
	if err != nil {
		t.Fatalf("reviewDecision: %v", err)
	}
	if dec != "REVIEW_REQUIRED" {
		t.Errorf("dec = %q, want REVIEW_REQUIRED", dec)
	}
}

// TestPrFromGL_StateAndMergeableMapping covers the mappings no fixture reaches:
// the conflict marker (the one value that says an MR cannot merge) and GitLab's
// merged/locked states, which normalize to the GitHub-shaped "closed".
func TestPrFromGL_StateAndMergeableMapping(t *testing.T) {
	if got := prFromGL(glMR{IID: 1, HasConflicts: true, DetailedMS: "mergeable"}).Mergeable; got != "conflicts" {
		t.Errorf("Mergeable = %q, want conflicts", got)
	}
	if got := prFromGL(glMR{IID: 2, MergeStatus: "cannot_be_merged"}).Mergeable; got != "cannot_be_merged" {
		t.Errorf("MergeStatus fallback = %q", got)
	}
	for in, want := range map[string]string{
		"opened": "open", "merged": "closed", "closed": "closed", "locked": "closed",
	} {
		if got := prFromGL(glMR{IID: 3, State: in}).State; got != want {
			t.Errorf("glPRState(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGLPostReviewComments_NoDiffRefsRefuses covers the guard that stops GitLab
// silently rejecting — or unpositioning — every comment. Without all three
// diff-ref SHAs a positioned discussion cannot be anchored, so refusing beats
// posting N unanchored comments.
func TestGLPostReviewComments_NoDiffRefsRefuses(t *testing.T) {
	const proj = "/projects/his%2Fhis"
	var posts int
	mux := http.NewServeMux()
	mux.HandleFunc(proj+"/merge_requests/6413", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"iid":6413,"state":"opened","diff_refs":{}}`))
	})
	mux.HandleFunc(proj+"/merge_requests/6413/discussions", func(w http.ResponseWriter, _ *http.Request) {
		posts++
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := newTestGLClient(t, srv.URL)

	err := c.PostReviewComments(context.Background(), 6413, []ReviewComment{{Path: "a.py", Line: 1, Body: "x"}})
	if err == nil || !strings.Contains(err.Error(), "no diff refs") {
		t.Fatalf("err = %v, want a no-diff-refs refusal", err)
	}
	if posts != 0 {
		t.Errorf("posted %d unanchored discussions", posts)
	}
	// Zero comments must make no call at all.
	if err := c.PostReviewComments(context.Background(), 6413, nil); err != nil {
		t.Errorf("empty comment set: %v", err)
	}
	if posts != 0 {
		t.Errorf("posted %d discussions for an empty set", posts)
	}
}

// TestGLListChanges_OverflowIsAnError pins the truncation guard. GitLab sets
// `overflow` when an MR is too large to return whole; treating that as a normal
// short answer fed a partial file list into the impact score as if it were the
// complete change.
func TestGLListChanges_OverflowIsAnError(t *testing.T) {
	const proj = "/projects/his%2Fhis"
	mux := http.NewServeMux()
	mux.HandleFunc(proj+"/merge_requests/6413/changes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"overflow": true, "changes": [
			{"old_path":"a.py","new_path":"a.py","diff":"@@ -1 +1 @@\n-a\n+b\n"}
		]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := newTestGLClient(t, srv.URL)

	if _, err := c.PRFiles(context.Background(), 6413); err == nil {
		t.Error("PRFiles returned a truncated file list as success")
	} else if !strings.Contains(err.Error(), "truncated") {
		t.Errorf("err = %v, want it to name the truncation", err)
	}
}

// TestGLPipelineState_ManualIsBlocked: a manual pipeline is BLOCKED awaiting a
// human-triggered job. Collapsing it to success reported a gated MR as READY
// with ci=SUCCESS, so an operator triaging on that pair would merge an MR whose
// gating stage never ran.
func TestGLPipelineState_ManualIsBlocked(t *testing.T) {
	for _, blocked := range []string{"manual", "waiting_for_resource", "scheduled", "created", "running"} {
		if got := glPipelineState(blocked); got != "pending" {
			t.Errorf("glPipelineState(%q) = %q, want pending", blocked, got)
		}
	}
	if got := ciFromPipeline(&glPipeline{Status: "manual"}); got != "PENDING" {
		t.Errorf("ciFromPipeline(manual) = %q, want PENDING", got)
	}
	if got := glPipelineState("success"); got != "success" {
		t.Errorf("glPipelineState(success) = %q", got)
	}
}

// TestGLPostReviewComments_PartialCountIsStructured pins the real landed count.
// res.Posted was counted before any network call, so a failure at comment 1 of
// N reported N posted; a failure BEFORE the loop reported N with zero posted.
func TestGLPostReviewComments_PartialCountIsStructured(t *testing.T) {
	const proj = "/projects/his%2Fhis"
	var calls int
	mux := http.NewServeMux()
	mux.HandleFunc(proj+"/merge_requests/6413", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(glMRGetJSON))
	})
	mux.HandleFunc(proj+"/merge_requests/6413/discussions", func(w http.ResponseWriter, _ *http.Request) {
		calls++
		if calls == 2 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	err := newTestGLClient(t, srv.URL).PostReviewComments(context.Background(), 6413, []ReviewComment{
		{Path: "a.py", Line: 1, Body: "one"},
		{Path: "b.py", Line: 2, Body: "two"},
		{Path: "c.py", Line: 3, Body: "three"},
	})
	var partial *PartialPostError
	if !errors.As(err, &partial) {
		t.Fatalf("err = %v, want a *PartialPostError carrying the count", err)
	}
	if partial.Posted != 1 || partial.Total != 3 {
		t.Errorf("Posted/Total = %d/%d, want 1/3", partial.Posted, partial.Total)
	}
}
