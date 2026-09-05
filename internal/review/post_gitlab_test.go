package review

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/forge"
)

func gitlabFindings() []Finding {
	return []Finding{
		{Rule: "nil-deref", Severity: SevWarning, Category: "correctness",
			File: "pkg/b.go", Line: 9, Message: "nil deref", Body: "x may be nil here"},
	}
}

// TestBatchesComments_DefaultsToGitHub is the guard against the zero-value trap:
// PostOptions and PostTarget are both built as struct literals in this repo, so
// a target that never names a provider must keep the batched GitHub shape.
func TestBatchesComments_DefaultsToGitHub(t *testing.T) {
	for _, tc := range []struct {
		provider string
		want     bool
	}{
		{"", true},
		{"github", true},
		{"gitlab", false},
		{"GitLab", false},
		{"  gitlab  ", false},
		{"unknown-forge", true},
	} {
		if got := (PostTarget{Provider: tc.provider}).BatchesComments(); got != tc.want {
			t.Errorf("provider %q: BatchesComments = %v, want %v", tc.provider, got, tc.want)
		}
	}
}

// TestPostFindings_DryRunGitLabShape checks the dry run renders what would
// actually be sent to GitLab — a positioned discussion, not a createReview
// comment. A dry run that showed the GitHub shape would be worse than useless
// on a GitLab target, since inspecting it is the whole point.
func TestPostFindings_DryRunGitLabShape(t *testing.T) {
	posted := withStubPoster(t)

	target := PostTarget{Provider: "gitlab", PRNumber: 6413}
	opts := PostOptions{DryRun: true, RefuseOnSecret: false}
	res, err := PostFindings(context.Background(), "/repo", target, gitlabFindings(), opts)
	if err != nil {
		t.Fatalf("PostFindings dry run: %v", err)
	}
	if len(*posted) != 0 {
		t.Fatal("dry run must not hit the forge poster")
	}
	if len(res.Payloads) != 1 {
		t.Fatalf("payloads = %d, want 1", len(res.Payloads))
	}

	p := res.Payloads[0]
	if _, ok := p["side"]; ok {
		t.Errorf("GitLab payload carries a GitHub `side` key: %v", p)
	}
	pos, ok := p["position"].(map[string]any)
	if !ok {
		t.Fatalf("GitLab payload has no position object: %v", p)
	}
	if pos["position_type"] != "text" {
		t.Errorf("position_type = %v, want text", pos["position_type"])
	}
	if pos["new_path"] != "pkg/b.go" {
		t.Errorf("new_path = %v", pos["new_path"])
	}
	if pos["new_line"] != 9 {
		t.Errorf("new_line = %v, want 9", pos["new_line"])
	}
	if body, _ := p["body"].(string); !strings.Contains(body, "x may be nil here") {
		t.Errorf("body = %q", body)
	}
}

// TestPostFindings_GitLabPartialCountIsReal pins the LANDED count, not the
// attempted one.
//
// res.Posted is incremented per finding BEFORE any network call, so it equals
// len(comments). The earlier contract left it untouched on the non-atomic path,
// reasoning that "the error carries the truth" — but that reported 2 posted for
// a run that landed 1, and N posted for one that landed none. The count now
// comes from the forge's typed *forge.PartialPostError.
func TestPostFindings_GitLabPartialCountIsReal(t *testing.T) {
	findings := append(gitlabFindings(), Finding{
		Rule: "n-plus-one", Severity: SevWarning, Category: "performance",
		File: "pkg/c.go", Line: 3, Message: "n+1", Body: "query in a loop",
	})
	target := PostTarget{Provider: "gitlab", PRNumber: 6413}

	// One comment landed before the failure.
	orig := postReviewComments
	postReviewComments = func(context.Context, string, int, []forge.ReviewComment) error {
		return &forge.PartialPostError{Posted: 1, Total: 2, Err: errors.New("400")}
	}
	t.Cleanup(func() { postReviewComments = orig })

	res, err := PostFindings(context.Background(), "/repo", target, findings, NewPostOptions())
	if err == nil {
		t.Fatal("want the forge error surfaced")
	}
	if res.Posted != 1 {
		t.Errorf("Posted = %d, want the real landed count 1 (not the attempted 2)", res.Posted)
	}

	// Nothing landed: an untyped error must report 0, never the attempted count.
	postReviewComments = func(context.Context, string, int, []forge.ReviewComment) error {
		return errors.New("connection refused")
	}
	res, err = PostFindings(context.Background(), "/repo", target, findings, NewPostOptions())
	if err == nil {
		t.Fatal("want the forge error surfaced")
	}
	if res.Posted != 0 {
		t.Errorf("Posted = %d on an untyped failure, want 0", res.Posted)
	}
}

// TestPostFindings_GitHubPartialFailureIsAtomic pins the contrasting GitHub
// behaviour: one createReview either lands whole or not at all.
func TestPostFindings_GitHubPartialFailureIsAtomic(t *testing.T) {
	orig := postReviewComments
	postReviewComments = func(_ context.Context, _ string, _ int, _ []forge.ReviewComment) error {
		return errors.New("boom")
	}
	t.Cleanup(func() { postReviewComments = orig })

	target := PostTarget{Provider: "github", PRNumber: 7}
	res, err := PostFindings(context.Background(), "/repo", target, gitlabFindings(), NewPostOptions())
	if err == nil {
		t.Fatal("want the forge error surfaced")
	}
	if res.Posted != 0 {
		t.Errorf("Posted = %d, want 0 for an atomic batched review", res.Posted)
	}
}

// TestPostFindings_DryRunGitLabLeftSide covers the LEFT branch of the dry-run
// payload. The live path covers LEFT already, so without this the two sides of
// one decision are asymmetrically covered — and the dry run is precisely what a
// user inspects before posting, where a wrong key anchors to the wrong file side.
func TestPostFindings_DryRunGitLabLeftSide(t *testing.T) {
	withStubPoster(t)
	c := forge.ReviewComment{Path: "pkg/b.go", Line: 9, Side: "LEFT", Body: "old side"}
	p := discussionCommentPayload(c)
	pos, ok := p["position"].(map[string]any)
	if !ok {
		t.Fatalf("no position: %v", p)
	}
	if pos["old_line"] != 9 {
		t.Errorf("old_line = %v, want 9", pos["old_line"])
	}
	if _, ok := pos["new_line"]; ok {
		t.Errorf("LEFT payload must not carry new_line: %v", pos)
	}
}

// TestPostFindings_GitLabReviewURL covers reviewURL's forge branch, which had
// zero hits: every other post test passes a repoDir with no resolvable remote,
// so PRWebURL returned "" and the assertion passed on the hardcoded github.com
// fallback. A regression making PRWebURL always return "" would have emitted a
// github.com URL for every GitLab MR unnoticed.
func TestPostFindings_GitLabReviewURL(t *testing.T) {
	orig := postReviewComments
	postReviewComments = func(context.Context, string, int, []forge.ReviewComment) error { return nil }
	t.Cleanup(func() { postReviewComments = orig })

	dir := t.TempDir()
	for _, a := range [][]string{{"init", "-q"}, {"remote", "add", "origin", "https://gitlab.example.com/his/his.git"}} {
		cmd := exec.Command("git", append([]string{"-C", dir}, a...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v: %v %s", a, err, out)
		}
	}

	res, err := PostFindings(context.Background(), dir,
		PostTarget{Provider: "gitlab", PRNumber: 6413}, gitlabFindings(), NewPostOptions())
	if err != nil {
		t.Fatalf("PostFindings: %v", err)
	}
	const want = "https://gitlab.example.com/his/his/-/merge_requests/6413"
	if res.ReviewURL != want {
		t.Errorf("ReviewURL = %q, want %q", res.ReviewURL, want)
	}
}
