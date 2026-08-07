package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// TestScopeForCWD_And_ReposInWorkspace exercises the per-session
// workspace resolution that underpins workspace isolation: a cwd is
// resolved to the workspace/project of the tracked repo that contains
// it, sibling repos sharing a workspace slug resolve together, a repo
// with no declared workspace is its own singleton workspace, and a cwd
// outside every tracked repo fails closed.
func TestScopeForCWD_And_ReposInWorkspace(t *testing.T) {
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")
	repoC := setupRepoDir(t, "repo-c") // no .gortex.yaml → singleton workspace

	// repo-a and repo-b share workspace "alpha"; repo-c declares none.
	require.NoError(t, os.WriteFile(filepath.Join(repoA, ".gortex.yaml"),
		[]byte("workspace: alpha\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoB, ".gortex.yaml"),
		[]byte("workspace: alpha\n"), 0o644))

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
			{Path: repoC, Name: "repo-c"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexScoped("", "") // empty scope → index every configured repo
	require.NoError(t, err)

	// cwd inside repo-a → workspace "alpha", home repo "repo-a".
	ws, _, prefix, ok := mi.ScopeForCWD(repoA)
	require.True(t, ok)
	assert.Equal(t, "alpha", ws)
	assert.Equal(t, "repo-a", prefix)

	// A nested subdirectory of repo-b still resolves to "alpha". The
	// path need not exist on disk — it is the agent's cwd, matched by
	// prefix against the tracked repo root.
	ws, _, prefix, ok = mi.ScopeForCWD(filepath.Join(repoB, "internal", "deep"))
	require.True(t, ok)
	assert.Equal(t, "alpha", ws)
	assert.Equal(t, "repo-b", prefix)

	// repo-c has no declared workspace → singleton workspace keyed on
	// the repo prefix.
	ws, _, prefix, ok = mi.ScopeForCWD(repoC)
	require.True(t, ok)
	assert.Equal(t, "repo-c", ws)
	assert.Equal(t, "repo-c", prefix)

	// A cwd outside every tracked repo must fail closed.
	_, _, _, ok = mi.ScopeForCWD(t.TempDir())
	assert.False(t, ok, "cwd outside every tracked repo must not resolve")

	// An empty cwd fails closed too.
	_, _, _, ok = mi.ScopeForCWD("")
	assert.False(t, ok)

	// ReposInWorkspace("alpha") is exactly {repo-a, repo-b}.
	alpha := mi.ReposInWorkspace("alpha")
	assert.True(t, alpha["repo-a"])
	assert.True(t, alpha["repo-b"])
	assert.False(t, alpha["repo-c"], "repo-c is not in workspace alpha")
	assert.Len(t, alpha, 2)

	// The singleton workspace "repo-c" contains only repo-c.
	singleton := mi.ReposInWorkspace("repo-c")
	assert.Equal(t, map[string]bool{"repo-c": true}, singleton)

	// An unknown workspace resolves to the empty set.
	assert.Empty(t, mi.ReposInWorkspace("does-not-exist"))
}

// TestScopeForCWD_WorkspaceRoot covers the reverse containment: a cwd
// that is not inside any tracked repo but CONTAINS tracked repos (an
// agent session started at a workspace root above its repos). Such a
// cwd binds to the contained repos' workspace when that workspace is
// unambiguous; repos spanning different workspaces keep failing closed
// — a single-workspace session scope cannot express the union.
func TestScopeForCWD_WorkspaceRoot(t *testing.T) {
	mkRepo := func(parent, name string) string {
		t.Helper()
		dir := filepath.Join(parent, name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
			[]byte("package main\n\nfunc Hello() {}\n"), 0o644))
		return dir
	}

	// Layout 1: one repo nested two levels under the workspace root.
	parentSingle := t.TempDir()
	app := mkRepo(filepath.Join(parentSingle, "projects"), "app")

	// Layout 2: two repos under one root, sharing workspace "shared".
	parentShared := t.TempDir()
	r1 := mkRepo(parentShared, "r1")
	r2 := mkRepo(parentShared, "r2")
	require.NoError(t, os.WriteFile(filepath.Join(r1, ".gortex.yaml"),
		[]byte("workspace: shared\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(r2, ".gortex.yaml"),
		[]byte("workspace: shared\n"), 0o644))

	// Layout 3: two repos under one root, each its own singleton workspace.
	parentMixed := t.TempDir()
	x := mkRepo(parentMixed, "x")
	y := mkRepo(parentMixed, "y")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: app, Name: "app"},
			{Path: r1, Name: "r1"},
			{Path: r2, Name: "r2"},
			{Path: x, Name: "x"},
			{Path: y, Name: "y"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexScoped("", "")
	require.NoError(t, err)

	// Workspace root above exactly one tracked repo: binds with the
	// full scope of that repo — the session behaves as if started
	// inside it (home repo included, so locality ranking still works).
	ws, proj, prefix, ok := mi.ScopeForCWD(parentSingle)
	require.True(t, ok, "workspace root containing one tracked repo must resolve")
	assert.Equal(t, "app", ws)
	assert.Equal(t, "app", proj)
	assert.Equal(t, "app", prefix)

	// Workspace root above two repos that share one workspace slug:
	// binds to the workspace, with no home repo (the session has no
	// single locality anchor).
	ws, proj, prefix, ok = mi.ScopeForCWD(parentShared)
	require.True(t, ok, "workspace root over a single shared workspace must resolve")
	assert.Equal(t, "shared", ws)
	assert.Empty(t, proj, "ambiguous project must stay empty")
	assert.Empty(t, prefix, "no home repo above multiple repos")

	// Workspace root above repos in DIFFERENT workspaces: fail closed —
	// one session scope cannot span two workspace boundaries.
	_, _, _, ok = mi.ScopeForCWD(parentMixed)
	assert.False(t, ok, "mixed-workspace parent must not resolve")

	// The forward direction is untouched: inside a repo still wins.
	ws, _, prefix, ok = mi.ScopeForCWD(filepath.Join(app, "internal"))
	require.True(t, ok)
	assert.Equal(t, "app", ws)
	assert.Equal(t, "app", prefix)
}
