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

// TestReconcileRepoCtx_EvictsOfflineDeletions simulates the exact B2
// scenario: daemon indexes a repo, saves its mtimes to a "snapshot",
// a file is deleted while the daemon is down, daemon restarts and
// reconciles via ReconcileRepoCtx. After reconcile, the deleted file's
// nodes must be absent from the graph.
func TestReconcileRepoCtx_EvictsOfflineDeletions(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeFile(t, filepath.Join(repoPath, "a.go"), "package main\nfunc Alpha() {}\n")
	writeFile(t, filepath.Join(repoPath, "b.go"), "package main\nfunc Beta() {}\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	// First "daemon run": index the repo, capture mtimes as if we were
	// writing a snapshot.
	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	meta := mi.GetMetadata("repo")
	require.NotNil(t, meta)
	priorMtimes := meta.FileMtimes

	// Before we "restart", delete b.go from disk. This mirrors the
	// user editing offline while the daemon is stopped.
	require.NoError(t, os.Remove(filepath.Join(repoPath, "b.go")))

	// Locate nodes for b.go before reconciliation — they exist in
	// the graph since the first pass indexed it.
	assert.NotEmpty(t, g.GetFileNodes("repo/b.go"), "b.go nodes must exist pre-reconcile")

	// Second "daemon run": fresh MultiIndexer, graph already populated
	// from the "snapshot", reconcile with prior mtimes.
	mi2 := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi2.ReconcileRepoCtx(context.Background(), config.RepoEntry{Path: repoPath, Name: "repo"}, priorMtimes)
	require.NoError(t, err)

	// The deleted file's nodes must be evicted — that's B2's contract.
	assert.Empty(t, g.GetFileNodes("repo/b.go"),
		"offline-deleted file's nodes must be evicted by reconciliation (B2)")
	// The surviving file's nodes must still be present.
	assert.NotEmpty(t, g.GetFileNodes("repo/a.go"),
		"unchanged file's nodes must survive reconciliation")
}

// TestReconcileRepoCtx_DoesNotDuplicateUnchanged is the B1 companion:
// reconciling a repo whose files haven't changed must be a no-op on the
// graph — no new nodes, no new edges, no duplicated secondary-index
// entries. Before Phase 1, the same scenario ran IndexCtx on top of a
// warm graph and doubled edges.
func TestReconcileRepoCtx_DoesNotDuplicateUnchanged(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeFile(t, filepath.Join(repoPath, "a.go"), "package main\nfunc Alpha() {}\nfunc Beta() {}\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	want := g.Stats()
	priorMtimes := mi.GetMetadata("repo").FileMtimes

	// Simulate restart: fresh MultiIndexer on the same graph, reconcile.
	mi2 := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi2.ReconcileRepoCtx(context.Background(), config.RepoEntry{Path: repoPath, Name: "repo"}, priorMtimes)
	require.NoError(t, err)

	got := g.Stats()
	assert.Equal(t, want.TotalNodes, got.TotalNodes,
		"reconciling unchanged files must not grow nodes")
	assert.Equal(t, want.TotalEdges, got.TotalEdges,
		"reconciling unchanged files must not grow edges (B1 regression)")
}

// TestReconcileAll_CatchesJanitorTargets runs ReconcileAll directly —
// the entry point the daemon's periodic janitor calls. Tests the same
// B2 invariant but through the public janitor API rather than the
// warmup path: once a file is deleted on disk, the next ReconcileAll
// must reflect that in the graph.
func TestReconcileAll_CatchesJanitorTargets(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeFile(t, filepath.Join(repoPath, "keep.go"), "package main\nfunc Keep() {}\n")
	writeFile(t, filepath.Join(repoPath, "drop.go"), "package main\nfunc Drop() {}\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	require.NotEmpty(t, g.GetFileNodes("repo/drop.go"))

	// Simulate an edit the watcher missed: delete drop.go on disk
	// without routing through IndexFile.
	require.NoError(t, os.Remove(filepath.Join(repoPath, "drop.go")))

	mi.ReconcileAll()

	assert.Empty(t, g.GetFileNodes("repo/drop.go"),
		"janitor must evict files deleted outside the watcher path")
	assert.NotEmpty(t, g.GetFileNodes("repo/keep.go"),
		"janitor must not disturb unchanged files")
}
