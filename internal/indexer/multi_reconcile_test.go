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
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// TestReconcileRepoCtx_EvictsOfflineDeletions simulates a warm restart:
// the daemon indexes a repo and records its mtimes, a file is deleted
// while the daemon is down, the daemon restarts and reconciles via
// ReconcileRepoCtx. After reconcile, the deleted file's nodes must be
// absent from the graph.
//
// The repo carries enough files that deleting one stays below the churn
// ratio that escalates to a whole-repo re-track, so this covers the scoped
// reconcile route that a restart normally takes.
func TestReconcileRepoCtx_EvictsOfflineDeletions(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeFile(t, filepath.Join(repoPath, "a.go"), "package main\nfunc Alpha() {}\n")
	writeFile(t, filepath.Join(repoPath, "b.go"), "package main\nfunc Beta() {}\n")
	writeFile(t, filepath.Join(repoPath, "c.go"), "package main\nfunc Gamma() {}\n")
	writeFile(t, filepath.Join(repoPath, "d.go"), "package main\nfunc Delta() {}\n")
	writeFile(t, filepath.Join(repoPath, "e.go"), "package main\nfunc Epsilon() {}\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	// First "daemon run": index the repo into the durable store and capture
	// mtimes, exactly as a daemon does before it shuts down.
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	g := graph.Store(s)
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	meta := mi.GetMetadata("repo")
	require.NotNil(t, meta)
	priorMtimes := mi.FileMtimes("repo")

	// Before we "restart", delete b.go from disk. This mirrors the
	// user editing offline while the daemon is stopped.
	require.NoError(t, os.Remove(filepath.Join(repoPath, "b.go")))

	// Locate nodes for b.go before reconciliation — they exist in
	// the graph since the first pass indexed it.
	assert.NotEmpty(t, g.GetFileNodes("repo/b.go"), "b.go nodes must exist pre-reconcile")

	// Second "daemon run": fresh MultiIndexer, graph already populated
	// from the "snapshot", reconcile with prior mtimes.
	mi2 := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi2.ReconcileRepoCtx(context.Background(), config.RepoEntry{Path: repoPath, Name: "repo"}, priorMtimes)
	require.NoError(t, err)

	// The deleted file's nodes must be evicted.
	assert.Empty(t, g.GetFileNodes("repo/b.go"),
		"offline-deleted file's nodes must be evicted by reconciliation")
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
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	want := g.Stats()
	priorMtimes := mi.FileMtimes("repo")

	// Simulate restart: fresh MultiIndexer on the same graph, reconcile.
	mi2 := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi2.ReconcileRepoCtx(context.Background(), config.RepoEntry{Path: repoPath, Name: "repo"}, priorMtimes)
	require.NoError(t, err)

	got := g.Stats()
	assert.Equal(t, want.TotalNodes, got.TotalNodes,
		"reconciling unchanged files must not grow nodes")
	assert.Equal(t, want.TotalEdges, got.TotalEdges,
		"reconciling unchanged files must not grow edges (B1 regression)")
}

// TestReconcileRepoCtx_RunsDerivedPassesForOfflineChange proves a restored
// repository consumes the modern pipeline's exact derived plan after an edit
// that happened while the coordinator was offline. The repo carries enough
// unchanged files that one new file stays on the scoped reconcile route
// instead of escalating to a whole-repo re-track.
func TestReconcileRepoCtx_RunsDerivedPassesForOfflineChange(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeFile(t, filepath.Join(repoPath, "base.go"), "package main\nfunc main() {}\n")
	writeFile(t, filepath.Join(repoPath, "one.go"), "package main\nfunc One() {}\n")
	writeFile(t, filepath.Join(repoPath, "two.go"), "package main\nfunc Two() {}\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	g := graph.Store(s)
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	priorMtimes := mi.FileMtimes("repo")

	writeFile(t, filepath.Join(repoPath, "shell.go"), `package main
import "os/exec"
func Run() error { return exec.Command("true").Run() }
`)

	mi2 := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	result, err := mi2.ReconcileRepoCtx(
		context.Background(), config.RepoEntry{Path: repoPath, Name: "repo"}, priorMtimes,
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.False(t, result.DerivedInvalidation.Empty(), "offline edit must return its exact derived plan")
	assert.True(t, result.DerivedInvalidation.Flags.Has(DerivedInvalidatesRuntime))
	assert.True(t, reconcileHasEdgeKind(g, graph.EdgeExecutesProcess),
		"snapshot reconciliation must consume the derived plan")
	require.NotNil(t, mi2.GetIndexer("repo"))
	assert.Equal(t, 4, mi2.GetIndexer("repo").TotalDetected())
}

func TestReconcileAll_RunsDerivedPassesForMissedChange(t *testing.T) {
	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeFile(t, filepath.Join(repoPath, "base.go"), "package main\nfunc main() {}\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	writeFile(t, filepath.Join(repoPath, "shell.go"), `package main
import "os/exec"
func Run() error { return exec.Command("true").Run() }
`)

	results := mi.ReconcileAllCtx(context.Background())
	require.Contains(t, results, "repo")
	require.NotNil(t, results["repo"])
	assert.False(t, results["repo"].DerivedInvalidation.Empty(),
		"janitor must retain the modern pipeline's exact derived plan")
	assert.True(t, reconcileHasEdgeKind(g, graph.EdgeExecutesProcess),
		"janitor must consume the plan after the batched repository loop")
}

func TestReconcileAllCtx_PreservesExistingBatchFlags(t *testing.T) {
	dir := t.TempDir()
	repoAPath := filepath.Join(dir, "repo-a")
	repoBPath := filepath.Join(dir, "repo-b")
	require.NoError(t, os.MkdirAll(repoAPath, 0o755))
	require.NoError(t, os.MkdirAll(repoBPath, 0o755))
	writeFile(t, filepath.Join(repoAPath, "a.go"), "package a\nfunc A() {}\n")
	writeFile(t, filepath.Join(repoBPath, "b.go"), "package b\nfunc B() {}\n")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: repoAPath, Name: "repo-a"},
		{Path: repoBPath, Name: "repo-b"},
	}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	idxA := mi.GetIndexer("repo-a")
	idxB := mi.GetIndexer("repo-b")
	require.NotNil(t, idxA)
	require.NotNil(t, idxB)

	// ReconcileAllCtx may run while an outer batch owns the shared flag. Give
	// the second repository an intentionally independent value so this also
	// catches per-repository state leaking across the reconciliation loop.
	mi.BeginBatch()
	t.Cleanup(mi.ResetBatch)
	idxB.SetDeferGlobalPasses(false)

	results := mi.ReconcileAllCtx(context.Background())
	require.Contains(t, results, "repo-a")
	require.Contains(t, results, "repo-b")
	assert.True(t, mi.deferGlobalPasses, "reconciliation must preserve the outer batch state")
	assert.True(t, idxA.deferGlobalPasses.Load(), "reconciliation must preserve repo-a's batch state")
	assert.False(t, idxB.deferGlobalPasses.Load(), "reconciliation must not leak repo-a's state into repo-b")
}

func reconcileHasEdgeKind(g graph.Store, kind graph.EdgeKind) bool {
	for _, edge := range g.AllEdges() {
		if edge != nil && edge.Kind == kind {
			return true
		}
	}
	return false
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
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
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
