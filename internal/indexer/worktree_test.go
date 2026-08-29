package indexer

import (
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// realpath resolves symlinks so macOS's /var → /private/var aliasing
// does not break a path comparison.
func realpath(t *testing.T, p string) string {
	t.Helper()
	abs, err := filepath.Abs(p)
	require.NoError(t, err)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

// initTestRepo creates a git repository with a single commit on branch,
// configured so the test never depends on the developer's global git
// config.
func initTestRepo(t *testing.T, dir, branch string) {
	t.Helper()
	runGit(t, dir, "init", "-q", "-b", branch)
	runGit(t, dir, "config", "user.email", "test@example.com")
	runGit(t, dir, "config", "user.name", "Test")
	runGit(t, dir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(dir, "a.go"), "package main\n")
	runGit(t, dir, "add", ".")
	runGit(t, dir, "commit", "-q", "-m", "init")
}

func TestResolveWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	main := t.TempDir()
	initTestRepo(t, main, "main")

	// The main checkout resolves to itself.
	mainInfo := ResolveWorktree(main)
	require.False(t, mainInfo.IsWorktree, "the main checkout is not a worktree")
	require.Equal(t, realpath(t, main), realpath(t, mainInfo.MainRepoPath))
	require.NotEmpty(t, mainInfo.GitCommonDir)

	// A linked worktree on a new branch.
	wt := filepath.Join(t.TempDir(), "feature-wt")
	runGit(t, main, "worktree", "add", "-q", "-b", "feature", wt)

	wtInfo := ResolveWorktree(wt)
	require.True(t, wtInfo.IsWorktree, "the linked worktree must be detected")
	require.Equal(t, realpath(t, main), realpath(t, wtInfo.MainRepoPath),
		"a worktree must resolve to the main repo it shares .git with")
	require.Equal(t, realpath(t, mainInfo.GitCommonDir), realpath(t, wtInfo.GitCommonDir),
		"the worktree and the main checkout share one .git common dir")
}

func TestResolveWorktree_NonGitDir(t *testing.T) {
	dir := t.TempDir()
	info := ResolveWorktree(dir)
	require.False(t, info.IsWorktree)
	require.Equal(t, realpath(t, dir), realpath(t, info.MainRepoPath))
	require.Empty(t, info.GitCommonDir)
}

// A worktree of a submodule shares a git dir under
// `<super>/.git/modules/<name>`, whose parent is `modules` rather than a
// `.git` — so the parent-of-commondir rule cannot find its checkout and
// every such worktree used to resolve to itself. That left two branches
// of one submodule in different checkout groups, which is what lets the
// resolver bind them to each other's copies of the same symbol.
func TestResolveWorktree_SubmoduleWorktree(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	// The repository that gets embedded as a submodule.
	sub := t.TempDir()
	initTestRepo(t, sub, "16.0")

	super := t.TempDir()
	initTestRepo(t, super, "main")
	// Adding a submodule from a local path needs the file protocol
	// explicitly re-enabled since git 2.38.
	runGit(t, super, "-c", "protocol.file.allow=always",
		"submodule", "add", "-q", sub, "src/local")
	runGit(t, super, "commit", "-q", "-m", "add submodule")

	// The submodule checkout carries a `.git` file with no `commondir`,
	// so it is a repository in its own right, not a worktree.
	subCheckout := filepath.Join(super, "src", "local")
	subInfo := ResolveWorktree(subCheckout)
	require.False(t, subInfo.IsWorktree, "a submodule is not a worktree")
	require.Equal(t, realpath(t, subCheckout), realpath(t, subInfo.MainRepoPath))

	wt := filepath.Join(t.TempDir(), "feature-wt")
	runGit(t, subCheckout, "worktree", "add", "-q", "-b", "feature", wt)

	wtInfo := ResolveWorktree(wt)
	require.True(t, wtInfo.IsWorktree, "a worktree of a submodule is a worktree")
	require.Equal(t, realpath(t, subCheckout), realpath(t, wtInfo.MainRepoPath),
		"a submodule worktree must resolve to the submodule's own checkout, "+
			"so both land in one checkout group")
}

func TestGitConfigCoreWorktree(t *testing.T) {
	cases := []struct {
		name   string
		config string
		want   string
	}{
		{
			name:   "submodule config",
			config: "[core]\n\tbare = false\n\tworktree = ../../../../src/local\n",
			want:   "../../../../src/local",
		},
		{
			name:   "no core section",
			config: "[remote \"origin\"]\n\turl = https://example.com/x.git\n",
			want:   "",
		},
		{
			name:   "worktree outside core is ignored",
			config: "[submodule \"x\"]\n\tworktree = /wrong\n",
			want:   "",
		},
		{
			name:   "last value wins, as git does",
			config: "[core]\n\tworktree = /first\n[core]\n\tworktree = /second\n",
			want:   "/second",
		},
		{
			name:   "section and key are case-insensitive",
			config: "[CORE]\n\tWorktree = /x\n",
			want:   "/x",
		},
		{
			name:   "quoted value",
			config: "[core]\n\tworktree = \"/quoted path\"\n",
			want:   "/quoted path",
		},
		{
			name:   "comments are skipped",
			config: "# [core]\n; worktree = /no\n[core]\n\tworktree = /yes\n",
			want:   "/yes",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			writeFile(t, path, tc.config)
			require.Equal(t, tc.want, gitConfigCoreWorktree(path))
		})
	}
}

func TestGitConfigCoreWorktree_MissingFile(t *testing.T) {
	require.Empty(t, gitConfigCoreWorktree(filepath.Join(t.TempDir(), "absent")))
}
