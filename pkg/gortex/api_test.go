package gortex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSample(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

func Hello() {}
func World() { Hello() }
`), 0o644))
	return dir
}

func TestEngine_IndexAndQuery(t *testing.T) {
	dir := writeSample(t)

	eng, err := New(WithWorkers(1))
	require.NoError(t, err)
	defer func() { require.NoError(t, eng.Close()) }()

	result, err := eng.Index(dir)
	require.NoError(t, err)

	assert.Equal(t, 1, result.FileCount)
	assert.Greater(t, result.NodeCount, 0)

	// FindSymbols.
	nodes := eng.FindSymbols("Hello")
	require.Len(t, nodes, 1)
	assert.Equal(t, "Hello", nodes[0].Name)

	// Stats.
	stats := eng.Stats()
	assert.Greater(t, stats.TotalNodes, 0)
	assert.Equal(t, stats.TotalNodes, result.NodeCount)
}

func TestEngine_CloseRemovesTemporaryStore(t *testing.T) {
	eng, err := New(WithWorkers(1))
	require.NoError(t, err)
	tmpDir := eng.tmpDir
	require.NotEmpty(t, tmpDir)

	require.NoError(t, eng.Close())
	_, statErr := os.Stat(tmpDir)
	assert.True(t, os.IsNotExist(statErr), "temp store directory should be gone after Close")

	// Close is idempotent.
	require.NoError(t, eng.Close())
}

func TestEngine_WithStorePathPersistsAcrossRuns(t *testing.T) {
	dir := writeSample(t)
	storePath := filepath.Join(t.TempDir(), "nested", "graph.sqlite")

	eng, err := New(WithWorkers(1), WithStorePath(storePath))
	require.NoError(t, err)
	_, err = eng.Index(dir)
	require.NoError(t, err)
	require.NoError(t, eng.Close())

	_, statErr := os.Stat(storePath)
	require.NoError(t, statErr, "store file should survive Close")

	reopened, err := New(WithWorkers(1), WithStorePath(storePath))
	require.NoError(t, err)
	defer func() { require.NoError(t, reopened.Close()) }()

	nodes := reopened.FindSymbols("Hello")
	require.Len(t, nodes, 1)
	assert.Equal(t, "Hello", nodes[0].Name)
}
