package lsp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// markAvailable force-marks a spec live in the router's PATH-lookup cache so
// the spawn path can be exercised on a CI box with no language servers
// installed.
func markAvailable(r *Router, name string) {
	r.availMu.Lock()
	r.avail[name] = true
	r.availMu.Unlock()
}

// TestNewRouter_EmptyDefaultStaysEmpty — filepath.Abs("") returns the
// process's working directory. A daemon tracking many repos is normally
// launched from none of them, so an empty default must stay empty rather
// than silently becoming the launch cwd.
func TestNewRouter_EmptyDefaultStaysEmpty(t *testing.T) {
	r := NewRouter("", zap.NewNop())
	defer r.Close()
	if got := r.DefaultWorkspace(); got != "" {
		cwd, _ := os.Getwd()
		t.Fatalf("empty default workspace resolved to %q (cwd is %q); it must stay empty", got, cwd)
	}
}

// TestForSpecWorkspace_RejectsMissingWorkspace — the workspace becomes the
// subprocess's working directory, so a root that does not exist must fail
// with a diagnosable error before the spawn, not with a bare chdir error
// from exec.
func TestForSpecWorkspace_RejectsMissingWorkspace(t *testing.T) {
	r := NewRouter("", zap.NewNop())
	defer r.Close()

	spec := &ServerSpec{Name: "fake-spec", Languages: []string{"csharp"}}
	markAvailable(r, spec.Name)

	phantom := filepath.Join(t.TempDir(), "not-a-real-repo", "SomeProject")
	_, err := r.ForSpecWorkspace(spec, phantom)
	if err == nil {
		t.Fatal("expected an error for a workspace that does not exist")
	}
	if !strings.Contains(err.Error(), phantom) {
		t.Fatalf("error should name the offending workspace, got: %v", err)
	}
	if got := r.Names(); len(got) != 0 {
		t.Fatalf("a rejected workspace must not land in the provider cache, got %v", got)
	}
}

// TestForSpecWorkspace_RejectedWorkspaceDoesNotPoisonSpec — a bad workspace
// is one repo's problem. It must not run through markSpawnFailed, which
// disables the spec for every other repo the daemon tracks for the rest of
// the session (issue #297: one mis-rooted query silently dropped C#
// enrichment daemon-wide).
func TestForSpecWorkspace_RejectedWorkspaceDoesNotPoisonSpec(t *testing.T) {
	r := NewRouter("", zap.NewNop())
	defer r.Close()

	spec := &ServerSpec{Name: "fake-spec", Languages: []string{"csharp"}}
	r.RegisterSpec(spec)
	markAvailable(r, spec.Name)

	if _, err := r.ForSpecWorkspace(spec, filepath.Join(t.TempDir(), "gone")); err == nil {
		t.Fatal("expected an error for a missing workspace")
	}
	if !r.SpecAvailable(spec.Name) {
		t.Fatal("a per-workspace failure must leave the spec available for other repos")
	}
}

// TestForSpecWorkspace_RejectsRelativeWorkspace — a relative root would be
// resolved against the daemon's own launch directory, producing a path under
// a repo the daemon does not track.
func TestForSpecWorkspace_RejectsRelativeWorkspace(t *testing.T) {
	r := NewRouter("", zap.NewNop())
	defer r.Close()

	spec := &ServerSpec{Name: "fake-spec", Languages: []string{"csharp"}}
	markAvailable(r, spec.Name)

	// "." is the dangerous shape: it exists, so absolutising it succeeds —
	// straight onto the daemon's launch directory. Only the relativeness
	// check can catch it.
	_, err := r.ForSpecWorkspace(spec, ".")
	if err == nil {
		t.Fatal("expected an error for a relative workspace that resolves against the process cwd")
	}
	if !strings.Contains(err.Error(), "not absolute") {
		t.Fatalf("error should call out the relative root, got: %v", err)
	}
	if got := r.Names(); len(got) != 0 {
		t.Fatalf("a rejected workspace must not land in the provider cache, got %v", got)
	}
}

// TestForSpecWorkspace_RejectsEmptyWorkspaceWithNoDefault — with no default
// workspace configured (the multi-repo daemon), omitting the per-repo root
// must fail loudly instead of falling back to the launch cwd.
func TestForSpecWorkspace_RejectsEmptyWorkspaceWithNoDefault(t *testing.T) {
	r := NewRouter("", zap.NewNop())
	defer r.Close()

	spec := &ServerSpec{Name: "fake-spec", Languages: []string{"csharp"}}
	markAvailable(r, spec.Name)

	if _, err := r.ForSpecWorkspace(spec, ""); err == nil {
		t.Fatal("expected an error when neither an explicit nor a default workspace is set")
	}
}

// TestEnsureClient_RejectsEmptyWorkspace — Provider.EnsureClient absolutises
// its argument, and filepath.Abs("") is the process cwd. Reject instead.
func TestEnsureClient_RejectsEmptyWorkspace(t *testing.T) {
	p := NewProvider("noop", nil, []string{"go"}, false, 1, zap.NewNop())
	if err := p.EnsureClient(""); err == nil {
		t.Fatal("expected an error for an empty workspace root")
	}
}

// TestWorkspaceKey_EmptyStaysEmpty — the cache key helper must not turn an
// unset workspace into the process cwd either, or Release would miss the
// entry the spawn path refused to create.
func TestWorkspaceKey_EmptyStaysEmpty(t *testing.T) {
	r := NewRouter("", zap.NewNop())
	defer r.Close()
	if key := r.workspaceKey("fake-spec", ""); key.workspace != "" {
		t.Fatalf("workspaceKey resolved an empty workspace to %q", key.workspace)
	}
}
