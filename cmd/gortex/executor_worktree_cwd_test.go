package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeLinkedWorktree lays out on disk what `git worktree add` leaves behind:
// the main checkout with a real `.git` directory, and a linked worktree whose
// `.git` is a FILE pointing at a per-worktree gitdir that carries a
// `commondir` back to the shared one. That indirection is the only thing
// separating a linked worktree from a submodule, so the fixture builds it
// exactly rather than approximating it.
func fakeLinkedWorktree(t *testing.T, dir string) (mainRepo, worktree string) {
	t.Helper()
	mainRepo = filepath.Join(dir, "main")
	worktree = filepath.Join(dir, "wt", "feature")
	wtGitDir := filepath.Join(mainRepo, ".git", "worktrees", "feature")

	for _, d := range []string{filepath.Join(mainRepo, ".git"), wtGitDir, worktree} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if err := os.WriteFile(filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+wtGitDir+"\n"), 0o644); err != nil {
		t.Fatalf("write worktree .git: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wtGitDir, "commondir"), []byte("../..\n"), 0o644); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
	return mainRepo, worktree
}

// TestResolveExecutor_RegisteredCheckoutCWDReachesTheDaemon pins the fix: a
// working directory that lies inside no tracked repo root but inside a
// registered checkout of a tracked family passes the pre-flight, and the call
// carries that cwd so the daemon-side binding can serve the worktree's view.
//
// Tracked-root membership alone answers "no" here, which is what refused every
// CLI query run from a linked worktree — with a remedy (`gortex track
// <worktree>`) that would have indexed the worktree a second time.
func TestResolveExecutor_RegisteredCheckoutCWDReachesTheDaemon(t *testing.T) {
	dir := t.TempDir()
	tracked := filepath.Join(dir, "main")
	worktree := filepath.Join(dir, "wt", "feature")

	stub := startStubDaemon(t, []string{tracked})
	stub.serveCheckouts(worktree)

	exec, err := resolveExecutor(worktree)
	if err != nil {
		t.Fatalf("a registered checkout cwd must pass the pre-flight: %v", err)
	}
	defer exec.Close()

	if _, ok := exec.(*daemonExecutor); !ok {
		t.Fatalf("the daemon-first path must return a *daemonExecutor, got %T", exec)
	}
	if hs := stub.seenMCPHandshake(); hs.CWD != worktree {
		t.Fatalf("the call must carry the worktree cwd, daemon saw %q want %q", hs.CWD, worktree)
	}
	probes := stub.seenCoverageProbes()
	if len(probes) == 0 || probes[0] != worktree {
		t.Fatalf("the pre-flight must ask the daemon about the cwd, probes=%v", probes)
	}
}

// TestRequireDaemonTool_FamilyWorktreeNeverSuggestsTrackingItself covers the
// worktree the daemon tracks the family of but has not bound to a checkout
// view. The call still fails — there is nothing to answer with — but the
// remedy must name the main checkout, never the worktree: tracking a linked
// worktree as its own repository is what the family model exists to avoid.
func TestRequireDaemonTool_FamilyWorktreeNeverSuggestsTrackingItself(t *testing.T) {
	dir := t.TempDir()
	mainRepo, worktree := fakeLinkedWorktree(t, dir)

	// The family is tracked; the daemon has registered no checkout for the
	// worktree, so file_coverage names none.
	startStubDaemon(t, []string{mainRepo})

	_, err := requireDaemonTool(worktree, "graph_stats", map[string]any{})
	if err == nil {
		t.Fatal("an unbound worktree must fail rather than answer from the wrong working copy")
	}
	msg := err.Error()
	if strings.Contains(msg, "gortex track "+worktree) {
		t.Fatalf("the error must not tell the user to track the worktree: %q", msg)
	}
	if !strings.Contains(msg, mainRepo) {
		t.Fatalf("the error must name the main checkout the worktree belongs to: %q", msg)
	}
}

// TestRequireDaemonTool_WorktreeRefusedByTheDaemonNamesTheReconcile covers the
// skew case: the pre-flight passes because a checkout is registered, but the
// daemon answering the call still refuses it. The remedy has to stay the
// family's — reconcile the main checkout — because the daemon demonstrably
// knows that repository already.
func TestRequireDaemonTool_WorktreeRefusedByTheDaemonNamesTheReconcile(t *testing.T) {
	dir := t.TempDir()
	mainRepo, worktree := fakeLinkedWorktree(t, dir)

	stub := startStubDaemon(t, []string{mainRepo})
	stub.serveCheckouts(worktree)
	stub.mcpError = &stubRPCError{Code: -32000, Message: "repository not tracked", ErrorCode: "repo_not_tracked"}

	_, err := requireDaemonTool(worktree, "graph_stats", map[string]any{})
	if err == nil {
		t.Fatal("a refused call must surface an error")
	}
	msg := err.Error()
	if strings.Contains(msg, "gortex track") {
		t.Fatalf("the daemon already tracks the family — the remedy must not be a track: %q", msg)
	}
	if !strings.Contains(msg, "gortex repos reconcile "+mainRepo) {
		t.Fatalf("the error must point at the family's reconcile: %q", msg)
	}
}

// TestRequireDaemonTool_UntrackedCWDKeepsTheTrackSuggestion pins the message a
// genuinely uncovered directory still gets. `gortex track <path>` is the right
// remedy there, and widening the worktree arm must not blunt it.
func TestRequireDaemonTool_UntrackedCWDKeepsTheTrackSuggestion(t *testing.T) {
	stranger := t.TempDir() // no .git anywhere above it, tracked by nobody
	startStubDaemon(t, []string{filepath.Join(t.TempDir(), "elsewhere")})

	_, err := requireDaemonTool(stranger, "graph_stats", map[string]any{})
	if err == nil {
		t.Fatal("an untracked cwd must fail")
	}
	want := fmt.Sprintf("the gortex daemon does not track %s — add it with `gortex track %s`", stranger, stranger)
	if err.Error() != want {
		t.Fatalf("untracked message changed:\n got %q\nwant %q", err.Error(), want)
	}
}
