package indexer

import (
	"context"
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

// There is no single-repo mode. A lone repo is the first tracked repo and
// its nodes carry its prefix from the first index, so tracking a second repo
// is not a transition — nothing about the first repo's ids changes.
//
// These tests pin that, plus the one thing repo COUNT still decides: whether
// an unqualified path is ambiguous.

func TestTrackRepo_FirstRepoIsPrefixedFromTheStart(t *testing.T) {
	dirA := setupRepoDir(t, "repo-a")
	dirB := setupRepoDir(t, "repo-b")

	cm := newTestConfigManager(t)
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())

	ctx := context.Background()
	_, err := mi.TrackRepoCtx(ctx, config.RepoEntry{Path: dirA, Name: "repo-a"})
	require.NoError(t, err)

	require.NotNil(t, g.GetNode("repo-a/main.go::Hello"), "the first repo is prefixed like any other")
	require.Nil(t, g.GetNode("main.go::Hello"), "no unprefixed id is ever minted")

	// Adding a second repo leaves the first one's ids untouched — there is
	// no re-mint, because there is nothing to migrate.
	_, err = mi.TrackRepoCtx(ctx, config.RepoEntry{Path: dirB, Name: "repo-b"})
	require.NoError(t, err)

	require.NotNil(t, g.GetNode("repo-a/main.go::Hello"), "repo-a's ids are unchanged by repo-b joining")
	require.NotNil(t, g.GetNode("repo-b/main.go::Hello"), "repo-b indexed with its prefix")

	root, ok := mi.RepoRoot("repo-a")
	require.True(t, ok)
	assert.Equal(t, dirA, root)
}

// UntrackRepo must actually remove the repo's nodes, or a later lone repo
// would resolve unqualified paths against stale identities from a checkout
// that is no longer tracked.
func TestUntrackRepo_EvictsTheReposNodes(t *testing.T) {
	dir := setupRepoDir(t, "repo-a")

	cm := newTestConfigManager(t)
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())

	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dir, Name: "repo-a"})
	require.NoError(t, err)
	require.NotNil(t, g.GetNode("repo-a/main.go::Hello"))

	nodes, _ := mi.UntrackRepo("repo-a")
	assert.Greater(t, nodes, 0, "untrack must evict the repo's nodes")
	assert.Nil(t, g.GetNode("repo-a/main.go::Hello"), "stale node must not survive untrack")

	// A different repo tracked afterwards must not inherit repo-a's identity.
	dirB := setupRepoDir(t, "repo-b")
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dirB, Name: "repo-b"})
	require.NoError(t, err)
	root, ok := mi.RepoRoot("")
	require.True(t, ok)
	assert.Equal(t, dirB, root)
}

// The empty prefix asks "the repo, unqualified". That is answerable only
// when exactly one repo is tracked; with two the question is ambiguous and
// must fail closed rather than pick one.
//
// It is answerable for ANY lone repo, including one that survived a 1→2→1
// track/untrack sequence. That used to fail closed, because a lone survivor
// minted under multi-repo rules was distinguishable from one minted in
// single-repo mode and stale unprefixed ids had to keep failing. With one id
// shape there is nothing left to disambiguate.
func TestRepoRoot_EmptyPrefixResolvesTheSoleRepo(t *testing.T) {
	dirA := setupRepoDir(t, "repo-a")
	dirB := setupRepoDir(t, "repo-b")

	cm := newTestConfigManager(t)
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())

	ctx := context.Background()
	_, err := mi.TrackRepoCtx(ctx, config.RepoEntry{Path: dirA, Name: "repo-a"})
	require.NoError(t, err)

	root, ok := mi.RepoRoot("")
	require.True(t, ok, "one tracked repo makes an unqualified path unambiguous")
	assert.Equal(t, dirA, root)
	assert.Equal(t, filepath.Join(dirA, "main.go"), mi.ResolveFilePath("main.go"))

	// Two repos: ambiguous, fail closed.
	_, err = mi.TrackRepoCtx(ctx, config.RepoEntry{Path: dirB, Name: "repo-b"})
	require.NoError(t, err)
	_, ok = mi.RepoRoot("")
	assert.False(t, ok, "two repos make an unqualified path ambiguous")
	assert.Empty(t, mi.ResolveFilePath("main.go"))

	// Back to one: answerable again, now for the prefixed survivor.
	mi.UntrackRepo("repo-a")
	root, ok = mi.RepoRoot("")
	require.True(t, ok, "the lone survivor of a track/untrack sequence is still the sole repo")
	assert.Equal(t, dirB, root)
}

// A repo whose directory name collides with one of its own top-level
// directories. Graph paths are prefixed, so `api/handlers.go` is
// unambiguously the prefix `api` plus `handlers.go` — the repo's own `api/`
// subdirectory appears in graph paths as `api/api/handlers.go`.
//
// Before prefixing was unconditional this needed a stat()-based collision
// guard, because a lone repo's graph paths were raw relative paths and
// `api/handlers.go` could mean either thing. Removing that guard is what
// makes the answer deterministic instead of dependent on what is on disk.
func TestResolveFilePath_RepoNameMatchingOwnSubdirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "api")
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "api"), 0o755))
	writeFile(t, filepath.Join(dir, "api", "handlers.go"), "package api\n\nfunc Handle() {}\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc Hello() {}\n")

	cm := newTestConfigManager(t)
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: dir, Name: "api"})
	require.NoError(t, err)

	// The prefixed graph path for the file inside the repo's own api/ dir.
	assert.Equal(t, filepath.Join(dir, "api", "handlers.go"), mi.ResolveFilePath("api/api/handlers.go"))
	// A single leading `api/` is the repo prefix, stripped.
	assert.Equal(t, filepath.Join(dir, "main.go"), mi.ResolveFilePath("api/main.go"))
	// An unqualified path anchors to the sole repo's root.
	assert.Equal(t, filepath.Join(dir, "main.go"), mi.ResolveFilePath("main.go"))
}
