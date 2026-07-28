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
)

// TestStandaloneIndexerKeepsTheEmptyPrefix is a deletion fence.
//
// The daemon now prefixes every repo, which makes it tempting to read every
// remaining `repoPrefix == ""` branch in the graph layer as single-repo-mode
// residue and delete it. That would be wrong. The STANDALONE Indexer —
// indexer.New with no SetRepoPrefix — mints unprefixed nodes by design, and
// it backs:
//
//	cmd/gortex/init.go, init_intake.go   (`gortex init`)
//	cmd/gortex/eval_*.go                 (the eval harnesses)
//	pkg/gortex/api.go                    (the public Go API)
//	internal/serverstack                 (the embedded single-process server)
//	bench/*
//
// Only MultiIndexer was made to always set a prefix. Indexer itself must keep
// accepting an empty one, and the exact-empty-prefix reads it depends on —
// GetRepoNodes(""), GetRepoNodesByLanguage("", lang), GetRepoContentNodes(""),
// EvictRepo("") on the SQLite backend — must keep working.
func TestStandaloneIndexerKeepsTheEmptyPrefix(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc Hello() {}\nfunc Caller() { Hello() }\n"), 0o644))

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{}, zap.NewNop())

	require.Empty(t, idx.RepoPrefix(), "a standalone Indexer carries no repo prefix")

	res, err := idx.Index(dir)
	require.NoError(t, err)
	require.Positive(t, res.NodeCount)

	// Unprefixed ids and unprefixed file paths.
	node := g.GetNode("main.go::Hello")
	require.NotNil(t, node, "a standalone Indexer mints unprefixed node ids")
	assert.Empty(t, node.RepoPrefix)
	assert.Equal(t, "main.go", node.FilePath)

	// The exact-empty-prefix reads must find them. If any of these ever
	// returns nothing, a graph-layer empty-prefix branch was deleted as
	// dead code and every standalone caller above silently went blind.
	assert.NotEmpty(t, g.GetRepoNodes(""),
		"GetRepoNodes(\"\") is the standalone Indexer's own repo, not dead single-repo-mode code")
	assert.NotEmpty(t, g.GetRepoNodesByLanguage("", "go"),
		"GetRepoNodesByLanguage(\"\", ...) likewise")
}
