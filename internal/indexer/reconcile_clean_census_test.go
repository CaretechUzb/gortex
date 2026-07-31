package indexer

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func TestChangedSinceMtimesCensusIncludesContractManifests(t *testing.T) {
	root := t.TempDir()
	manifest := filepath.Join(root, "go.mod")
	require.NoError(t, os.WriteFile(manifest, []byte("module example.com/sample\n"), 0o644))

	idx := newTestIndexer(graph.New())
	idx.SetFileMtimes(map[string]int64{"go.mod": 1})

	changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
	require.NoError(t, err)
	assert.Equal(t, []string{"go.mod"}, changed)
	assert.Empty(t, deleted)
	assert.Equal(t, 1, detected)

	require.NoError(t, os.Remove(manifest))
	changed, deleted, detected, err = idx.changedSinceMtimesCensus(root)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Equal(t, []string{"go.mod"}, deleted)
	assert.Zero(t, detected)
}

func TestChangedSinceMtimesCensusDeletesNewlyExcludedTrackedFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "ignored.go")
	require.NoError(t, os.WriteFile(path, []byte("package sample\n"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	idx := newTestIndexer(graph.New())
	idx.SetFileMtimes(map[string]int64{"ignored.go": info.ModTime().UnixNano()})
	idx.SetExcludePatterns([]string{"ignored.go"})

	changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Equal(t, []string{"ignored.go"}, deleted)
	assert.Zero(t, detected)
}

func TestCleanCensusResultBootstrapsNonPersistentSearch(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:       "function::Alpha",
		Kind:     graph.KindFunction,
		Name:     "Alpha",
		FilePath: "a.go",
	})
	idx := newTestIndexer(g)
	idx.search = search.NewBM25()
	idx.SetFileMtimes(map[string]int64{"a.go": 1})

	result := idx.cleanCensusResult(1, time.Now())

	require.NotNil(t, result)
	assert.Equal(t, 1, result.FileCount)
	assert.Equal(t, 1, result.NodeCount)
	assert.Equal(t, 1, idx.TotalDetected())
	assert.Equal(t, 1, idx.search.Count())
	require.NotEmpty(t, idx.search.Search("Alpha", 10))
}

func TestReconcileRepoCtxUsesCleanCensusNoOp(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Alpha() {}\n")
	entry := config.RepoEntry{Path: root, Name: "repo"}
	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{entry}

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	seed := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = seed.IndexAll()
	require.NoError(t, err)
	prior := seed.GetIndexer("repo").FileMtimes()
	before := store.Stats()

	core, logs := observer.New(zap.DebugLevel)
	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewBM25(), cm, zap.New(core))
	result, err := restarted.ReconcileRepoCtx(t.Context(), entry, prior)
	require.NoError(t, err)
	require.NotNil(t, result)

	after := store.Stats()
	assert.Zero(t, result.StaleFileCount)
	assert.Zero(t, result.DeletedFileCount)
	assert.False(t, result.FullRetrack)
	assert.Equal(t, before.TotalNodes, after.TotalNodes)
	assert.Equal(t, before.TotalEdges, after.TotalEdges)
	assert.Equal(t, len(prior), result.FileCount)
	assert.Equal(t, result.FileCount, restarted.GetIndexer("repo").TotalDetected())

	entries := logs.FilterMessage("daemon: reconciled repo from snapshot").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "census_noop", entries[0].ContextMap()["route"])
}

func BenchmarkCleanReconcileDiscovery(b *testing.B) {
	root := b.TempDir()
	const files = 512
	mtimes := make(map[string]int64, files)
	for i := 0; i < files; i++ {
		rel := fmt.Sprintf("pkg/file-%04d.go", i)
		path := filepath.Join(root, filepath.FromSlash(rel))
		require.NoError(b, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(b, os.WriteFile(path, []byte("package sample\n"), 0o644))
		info, err := os.Stat(path)
		require.NoError(b, err)
		mtimes[rel] = info.ModTime().UnixNano()
	}
	idx := newTestIndexer(graph.New())
	idx.SetFileMtimes(mtimes)

	b.Run("authoritative_census", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
			if err != nil || len(changed) != 0 || len(deleted) != 0 {
				b.Fatalf("clean census: changed=%d deleted=%d err=%v", len(changed), len(deleted), err)
			}
			idx.cleanCensusResult(detected, time.Now())
		}
	})

	b.Run("legacy_double_walk", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			changed, deleted, _, err := idx.changedSinceMtimesCensus(root)
			if err != nil || len(changed) != 0 || len(deleted) != 0 {
				b.Fatalf("clean census: changed=%d deleted=%d err=%v", len(changed), len(deleted), err)
			}
			result, err := idx.incrementalReindexPathsMode(root, nil, incrementalPathMode{detectDeletions: true})
			if err != nil || result.StaleFileCount != 0 || result.DeletedFileCount != 0 {
				b.Fatalf("legacy no-op: result=%+v err=%v", result, err)
			}
		}
	})
}
