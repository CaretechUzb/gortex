package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func TestReconcileRepoCtxKeepsMerkleFallbackForCleanMtimeCensus(t *testing.T) {
	t.Setenv("GORTEX_MERKLE", "1")
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	writeFile(t, path, "package sample\nfunc Alpha() {}\n")
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

	indexedInfo, err := os.Stat(path)
	require.NoError(t, err)
	writeFile(t, path, "package sample\nfunc Bravo() {}\n")
	require.NoError(t, os.Chtimes(path, indexedInfo.ModTime(), indexedInfo.ModTime()))

	// A restored mtime makes the cheap census look clean. If the persisted
	// Merkle baseline is missing, the preserving full-root incremental path
	// hashes the file and surfaces it. The direct census no-op must therefore
	// remain disabled for every Merkle-enabled repository.
	require.NoError(t, os.Remove(merkleTreeFile(root)))
	core, logs := observer.New(zap.DebugLevel)
	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewBM25(), cm, zap.New(core))
	result, err := restarted.ReconcileRepoCtx(t.Context(), entry, prior)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, result.StaleFileCount)
	assert.NotEmpty(t, store.FindNodesByName("Bravo"))
	assert.Empty(t, store.FindNodesByName("Alpha"))

	entries := logs.FilterMessage("daemon: reconciled repo from snapshot").All()
	require.Len(t, entries, 1)
	assert.Equal(t, "incremental", entries[0].ContextMap()["route"])
}
