package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// gitRepoWithWorktree creates a real repository plus a linked worktree of
// it. Real git plumbing, not a fixture: the whole point of the grouping is
// that it reads the same `.git` indirection `git worktree` writes, and a
// hand-built fake would prove nothing about that.
func gitRepoWithWorktree(t *testing.T) (main, worktree string) {
	t.Helper()
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git %v unavailable: %v (%s)", args, err, out)
		}
	}

	root := t.TempDir()
	main = filepath.Join(root, "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}
	git(main, "init", "-q")
	if err := os.WriteFile(filepath.Join(main, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(main, "add", ".")
	git(main, "commit", "-qm", "init")

	worktree = filepath.Join(root, "wt")
	git(main, "worktree", "add", "-q", "-b", "bench", worktree)
	return main, worktree
}

func TestCheckoutGroups_GroupsAWorktreeWithItsRepository(t *testing.T) {
	main, worktree := gitRepoWithWorktree(t)
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{
		"local":       {RepoPrefix: "local", RootPath: main},
		"local-bench": {RepoPrefix: "local-bench", RootPath: worktree},
	}}

	groups := mi.checkoutGroups()
	if len(groups) != 2 {
		t.Fatalf("both prefixes must be grouped, got %v", groups)
	}
	if groups["local"] != groups["local-bench"] {
		t.Fatalf("a worktree and its repository must share a group: %v", groups)
	}
}

// Only prefixes that actually share a checkout are published. This is
// what lets every consumer short-circuit on HasCheckoutGroups() before
// doing any per-edge work in the overwhelmingly common workspace.
func TestCheckoutGroups_LoneRepositoryPublishesNothing(t *testing.T) {
	main, _ := gitRepoWithWorktree(t)
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{
		"local": {RepoPrefix: "local", RootPath: main},
	}}

	if groups := mi.checkoutGroups(); len(groups) != 0 {
		t.Fatalf("a repository with no tracked worktree groups with nothing, got %v", groups)
	}
}

// Two unrelated directories holding identical files are two repositories,
// not one checkout twice. Grouping them would suppress genuine cross-repo
// edges — the failure mode that matters most here.
func TestCheckoutGroups_UnrelatedRepositoriesStayDistinct(t *testing.T) {
	mainA, _ := gitRepoWithWorktree(t)
	mainB, _ := gitRepoWithWorktree(t)
	mi := &MultiIndexer{repos: map[string]*RepoMetadata{
		"a": {RepoPrefix: "a", RootPath: mainA},
		"b": {RepoPrefix: "b", RootPath: mainB},
	}}

	if groups := mi.checkoutGroups(); len(groups) != 0 {
		t.Fatalf("independent repositories must not be grouped, got %v", groups)
	}
}

func TestPublishCheckoutGroups_ReachesTheStore(t *testing.T) {
	main, worktree := gitRepoWithWorktree(t)
	g := graph.New()
	mi := &MultiIndexer{graph: g, repos: map[string]*RepoMetadata{
		"local":       {RepoPrefix: "local", RootPath: main},
		"local-bench": {RepoPrefix: "local-bench", RootPath: worktree},
	}}

	mi.publishCheckoutGroups()
	if !graph.SiblingCheckouts(g, "local", "local-bench") {
		t.Fatal("the store must see the worktree relationship after publication")
	}

	// Untracking the worktree has to clear it, not leave a phantom sibling.
	delete(mi.repos, "local-bench")
	mi.publishCheckoutGroups()
	if graph.SiblingCheckouts(g, "local", "local-bench") {
		t.Fatal("an untracked worktree must stop being a sibling")
	}
}
