package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// TestChangedSinceMtimes_Census verifies the accumulating census returns the
// exact changed / deleted sets across the five mutation shapes — add, edit,
// delete, rename, and an inert touch (a bare mtime bump with identical
// content still counts as changed at this layer; content-hash short-circuit
// is a separate concern handled higher up).
func TestChangedSinceMtimes_Census(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "edit.go"), "package main\n\nfunc Edit() {}\n")
	writeFile(t, filepath.Join(dir, "del.go"), "package main\n\nfunc Del() {}\n")
	writeFile(t, filepath.Join(dir, "old.go"), "package main\n\nfunc Ren() {}\n")
	writeFile(t, filepath.Join(dir, "touch.go"), "package main\n\nfunc Touch() {}\n")
	writeFile(t, filepath.Join(dir, "stable.go"), "package main\n\nfunc Stable() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	// Baseline: nothing changed on disk since the index.
	changed, deleted, err := idx.ChangedSinceMtimes(dir)
	require.NoError(t, err)
	assert.Empty(t, changed, "an unchanged tree reports no changed files")
	assert.Empty(t, deleted, "an unchanged tree reports no deleted files")

	// Mutate: one added, one edited, one deleted, one renamed, one touched.
	writeFile(t, filepath.Join(dir, "add.go"), "package main\n\nfunc Add() {}\n")
	bumpMtime(t, filepath.Join(dir, "edit.go"), "package main\n\nfunc Edit() {}\n\nfunc More() {}\n")
	require.NoError(t, os.Remove(filepath.Join(dir, "del.go")))
	require.NoError(t, os.Rename(filepath.Join(dir, "old.go"), filepath.Join(dir, "new.go")))
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "touch.go"), future, future))

	changed, deleted, err = idx.ChangedSinceMtimes(dir)
	require.NoError(t, err)

	assert.ElementsMatch(t, []string{"add.go", "edit.go", "new.go", "touch.go"}, changed,
		"changed = added + edited + renamed-new + inert-touched")
	assert.ElementsMatch(t, []string{"del.go", "old.go"}, deleted,
		"deleted = removed file + renamed-old path")
	assert.NotContains(t, changed, "stable.go", "an untouched file is not reported")
}

// TestReconcileRepoCtx_ScopedEqualsFullIndex is the golden-equivalence gate:
// a warm-restart reconcile that routes through the scoped incremental path
// (a small mutation set in a large repo) must produce a graph identical to a
// from-scratch full index of the final on-disk tree — same node IDs, same
// (from, to, kind) edge triples — ignoring volatile per-edge/node meta.
func TestReconcileRepoCtx_ScopedEqualsFullIndex(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))

	// A repo big enough that the four-file mutation below stays well under
	// the 40% churn threshold, so the reconcile takes the SCOPED route this
	// gate exists to prove — not a whole-repo re-track.
	for i := 0; i < 20; i++ {
		writeFile(t, filepath.Join(repoPath, fmt.Sprintf("f%02d.go", i)),
			fmt.Sprintf("package repo\n\nfunc F%02d() {}\n", i))
	}
	writeFile(t, filepath.Join(repoPath, "edited.go"), "package repo\n\nfunc Edited() {}\n")
	writeFile(t, filepath.Join(repoPath, "deleted.go"), "package repo\n\nfunc Deleted() {}\n")
	writeFile(t, filepath.Join(repoPath, "renold.go"), "package repo\n\nfunc Renamed() {}\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	// First "daemon run": index onto a disk-backed store, capture the
	// warm-restart mtime snapshot.
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	mi := NewMultiIndexer(graph.Store(s), newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	priorMtimes := mi.GetMetadata("repo").FileMtimes
	require.NotEmpty(t, priorMtimes)

	// Mutate on disk while the daemon is "down": add, edit, delete, rename.
	writeFile(t, filepath.Join(repoPath, "added.go"), "package repo\n\nfunc Added() {}\n")
	bumpMtime(t, filepath.Join(repoPath, "edited.go"), "package repo\n\nfunc Edited() {}\n\nfunc EditedTwo() {}\n")
	require.NoError(t, os.Remove(filepath.Join(repoPath, "deleted.go")))
	require.NoError(t, os.Rename(filepath.Join(repoPath, "renold.go"), filepath.Join(repoPath, "rennew.go")))

	// Second "daemon run": a fresh MultiIndexer over the same persisted
	// store reconciles from the snapshot.
	mi2 := NewMultiIndexer(graph.Store(s), newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	res, err := mi2.ReconcileRepoCtx(context.Background(), config.RepoEntry{Path: repoPath, Name: "repo"}, priorMtimes)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.FullRetrack,
		"a four-file mutation in a 23-file repo must route scoped, not full-retrack")

	// Sanity: the deltas actually landed on the reconciled store.
	require.NotEmpty(t, s.GetFileNodes("repo/added.go"), "added file must be present after reconcile")
	require.NotEmpty(t, s.GetFileNodes("repo/rennew.go"), "renamed-new file must be present after reconcile")
	require.Empty(t, s.GetFileNodes("repo/deleted.go"), "deleted file must be evicted after reconcile")
	require.Empty(t, s.GetFileNodes("repo/renold.go"), "renamed-old file must be evicted after reconcile")

	// Golden: a from-scratch full index of the final tree, on a fresh disk
	// store through an equivalent single-repo config (so node IDs prefix the
	// same way), must be byte-identical to the reconciled graph.
	cfgPath2 := filepath.Join(dir, "config2.yaml")
	gc2 := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc2.SetConfigPath(cfgPath2)
	require.NoError(t, gc2.Save())
	cm2, err := config.NewConfigManager(cfgPath2)
	require.NoError(t, err)

	s2, err := store_sqlite.Open(filepath.Join(t.TempDir(), "golden.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })
	miGold := NewMultiIndexer(graph.Store(s2), newTestRegistry(), search.NewBM25(), cm2, zap.NewNop())
	_, err = miGold.IndexAll()
	require.NoError(t, err)

	gold := canonicalGraph(graph.Store(s2))
	got := canonicalGraph(graph.Store(s))
	require.NotEmpty(t, gold, "the golden index must not be empty")
	assert.Equal(t, gold, got,
		"scoped warm-restart reconcile must converge to the same graph as a from-scratch full index")
}

// TestReconcileRepoCtx_Routing pins the three routing decisions the census
// drives on a disk-backed store: an empty census takes the incremental
// no-op (never a full re-track), a large-fraction churn falls back to a full
// re-track, and GORTEX_WARMUP_FULL_RETRACK forces a full re-track even when
// nothing changed.
func TestReconcileRepoCtx_Routing(t *testing.T) {
	// indexAndSnapshot indexes repoPath onto a fresh disk store and returns
	// the store, config manager, and the captured mtime snapshot.
	indexAndSnapshot := func(t *testing.T, repoPath string) (*store_sqlite.Store, *config.ConfigManager, map[string]int64) {
		t.Helper()
		cfgPath := filepath.Join(filepath.Dir(repoPath), "config.yaml")
		gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
		gc.SetConfigPath(cfgPath)
		require.NoError(t, gc.Save())
		cm, err := config.NewConfigManager(cfgPath)
		require.NoError(t, err)

		s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = s.Close() })

		mi := NewMultiIndexer(graph.Store(s), newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
		_, err = mi.IndexAll()
		require.NoError(t, err)
		return s, cm, mi.GetMetadata("repo").FileMtimes
	}

	reconcile := func(t *testing.T, s *store_sqlite.Store, cm *config.ConfigManager, repoPath string, prior map[string]int64) *IndexResult {
		t.Helper()
		mi := NewMultiIndexer(graph.Store(s), newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
		res, err := mi.ReconcileRepoCtx(context.Background(), config.RepoEntry{Path: repoPath, Name: "repo"}, prior)
		require.NoError(t, err)
		require.NotNil(t, res)
		return res
	}

	t.Run("empty census takes the incremental no-op", func(t *testing.T) {
		dir := t.TempDir()
		repoPath := filepath.Join(dir, "repo")
		require.NoError(t, os.MkdirAll(repoPath, 0o755))
		writeFile(t, filepath.Join(repoPath, "a.go"), "package main\n\nfunc A() {}\n")
		writeFile(t, filepath.Join(repoPath, "b.go"), "package main\n\nfunc B() {}\n")

		s, cm, prior := indexAndSnapshot(t, repoPath)
		// Nothing changed on disk since the snapshot.
		res := reconcile(t, s, cm, repoPath, prior)
		assert.False(t, res.FullRetrack, "an unchanged repo must not full-retrack")
		assert.Equal(t, 0, res.StaleFileCount, "an unchanged repo re-indexes nothing")
	})

	t.Run("large-fraction churn falls back to full retrack", func(t *testing.T) {
		dir := t.TempDir()
		repoPath := filepath.Join(dir, "repo")
		require.NoError(t, os.MkdirAll(repoPath, 0o755))
		writeFile(t, filepath.Join(repoPath, "a.go"), "package main\n\nfunc A() {}\n")
		writeFile(t, filepath.Join(repoPath, "b.go"), "package main\n\nfunc B() {}\n")

		s, cm, prior := indexAndSnapshot(t, repoPath)
		// One of two files changes: 50% churn, over the 40% threshold.
		bumpMtime(t, filepath.Join(repoPath, "a.go"), "package main\n\nfunc A() {}\n\nfunc A2() {}\n")
		res := reconcile(t, s, cm, repoPath, prior)
		assert.True(t, res.FullRetrack, "churn above 40% of the repo must full-retrack")
	})

	t.Run("env override forces full retrack", func(t *testing.T) {
		t.Setenv("GORTEX_WARMUP_FULL_RETRACK", "1")
		dir := t.TempDir()
		repoPath := filepath.Join(dir, "repo")
		require.NoError(t, os.MkdirAll(repoPath, 0o755))
		writeFile(t, filepath.Join(repoPath, "a.go"), "package main\n\nfunc A() {}\n")
		writeFile(t, filepath.Join(repoPath, "b.go"), "package main\n\nfunc B() {}\n")

		s, cm, prior := indexAndSnapshot(t, repoPath)
		// Nothing changed, but the override forces a whole-repo re-track.
		res := reconcile(t, s, cm, repoPath, prior)
		assert.True(t, res.FullRetrack, "GORTEX_WARMUP_FULL_RETRACK=1 must force a full re-track")
	})
}
