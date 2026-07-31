package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestMerkleIncrementalFallbackSeesSameMtimeChangeWithoutBaseline(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "main.go")
	writeFile(t, path, "package sample\nfunc Alpha() {}\n")

	idx := newTestIndexer(graph.New())
	idx.config.Merkle = true
	_, err := idx.Index(root)
	require.NoError(t, err)

	indexedInfo, err := os.Stat(path)
	require.NoError(t, err)
	writeFile(t, path, "package sample\nfunc Bravo() {}\n")
	require.NoError(t, os.Chtimes(path, indexedInfo.ModTime(), indexedInfo.ModTime()))

	// A restored mtime makes the cheap census look clean. With no persisted
	// Merkle baseline, however, the full-root incremental path must hash the
	// file and surface it. ReconcileRepoCtx therefore cannot use census_noop
	// for a Merkle-enabled repository.
	require.NoError(t, os.Remove(merkleTreeFile(root)))
	changed, deleted, detected, err := idx.changedSinceMtimesCensus(root)
	require.NoError(t, err)
	assert.Empty(t, changed)
	assert.Empty(t, deleted)
	assert.Equal(t, 1, detected)

	result, err := idx.incrementalReindexPathsMode(root, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 1, result.StaleFileCount)
}
