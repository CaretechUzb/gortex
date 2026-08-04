package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestFullTreeReconcileEvictsFilesTheMtimeLedgerForgot is the regression for
// the orphan population index_health could not see: symbols whose file is gone
// from disk and whose mtime record is gone from the ledger, leaving nothing
// that any freshness signal keys on. Before the graph-derived candidate sweep,
// a full-tree reconcile walked fileMtimes only, so those nodes survived every
// pass and kept answering searches forever.
func TestFullTreeReconcileEvictsFilesTheMtimeLedgerForgot(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "live.go"), "package sample\n\nfunc Live() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(root)
	require.NoError(t, err)

	// A file the daemon indexed and then lost track of: its nodes and its
	// compact file row are in the graph, but the deletion was never witnessed
	// and no mtime record survives to point at it.
	deleted := filepath.Join(root, "tmpcopy", "gamma.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(deleted), 0o755))
	writeFile(t, deleted, "package tmpcopy\n\nfunc GammaOrphan() {}\n")
	require.NoError(t, idx.IndexFile(deleted))

	require.NotEmpty(t, g.GetFileNodes("tmpcopy/gamma.go"), "precondition: orphan file indexed")

	require.NoError(t, os.RemoveAll(filepath.Join(root, "tmpcopy")))
	idx.mtimeMu.Lock()
	delete(idx.fileMtimes, "tmpcopy/gamma.go")
	idx.mtimeMu.Unlock()

	result, err := idx.incrementalReindexPathsMode(root, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	assert.Equal(t, 1, result.DeletedFileCount, "orphan file should be evicted")
	assert.Empty(t, g.GetFileNodes("tmpcopy/gamma.go"), "orphan nodes should be gone")

	rows, err := g.FileMetasForRepo("")
	require.NoError(t, err)
	for _, row := range rows {
		assert.NotEqual(t, "tmpcopy/gamma.go", row.FilePath, "orphan file row should be gone")
	}

	assert.NotEmpty(t, g.GetFileNodes("live.go"), "surviving file must not be swept")
}

// TestFullTreeReconcileKeepsIndexedFilesTheWalkDoesNotClaim guards the sweep's
// safety gate. The disk walk only admits files whose language is recognised, so
// the graph legitimately holds file rows the walk never yields. Those must be
// preserved: the sweep contributes candidates, and only a failed stat decides a
// deletion.
func TestFullTreeReconcileKeepsIndexedFilesTheWalkDoesNotClaim(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "live.go"), "package sample\n\nfunc Live() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(root)
	require.NoError(t, err)

	// A real file on disk that the Go-only walk will not pick up, recorded in
	// the graph the way an artifact or a foreign-language pass would record it.
	writeFile(t, filepath.Join(root, "schema.sql"), "CREATE TABLE t (id INT);\n")
	g.AddNode(&graph.Node{
		ID:       "schema.sql",
		Kind:     graph.KindFile,
		Name:     "schema.sql",
		FilePath: "schema.sql",
	})
	require.NoError(t, g.SetFileMetas("", []graph.FileMetaRow{{FilePath: "schema.sql", NodeCount: 1}}))

	result, err := idx.incrementalReindexPathsMode(root, nil, incrementalPathMode{detectDeletions: true})
	require.NoError(t, err)
	assert.Zero(t, result.DeletedFileCount, "a file still on disk must never be swept")
	assert.NotEmpty(t, g.GetFileNodes("schema.sql"))
}

func TestGraphPathRelKeyRejectsForeignRepoPaths(t *testing.T) {
	idx := newTestIndexer(graph.New())
	idx.SetRepoPrefix("mine")

	rel, owned := idx.graphPathRelKey("mine/internal/foo.go")
	assert.True(t, owned)
	assert.Equal(t, "internal/foo.go", rel)

	_, owned = idx.graphPathRelKey("theirs/internal/foo.go")
	assert.False(t, owned, "another repo's path must never resolve against this root")

	_, owned = idx.graphPathRelKey("mine/")
	assert.False(t, owned)

	unprefixed := newTestIndexer(graph.New())
	rel, owned = unprefixed.graphPathRelKey("internal/foo.go")
	assert.True(t, owned)
	assert.Equal(t, "internal/foo.go", rel)
}

// A full-tree walk that yields nothing is a vanished root far more often than
// an emptied repository. The graph-derived sweep must not turn that into a
// whole-repository eviction.
func TestGraphSweepDeclinesWhenTheWalkFoundNothing(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "live.go"), "package sample\n\nfunc Live() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(root)
	require.NoError(t, err)

	assert.Empty(t, idx.indexedFilesAbsentFromDisk(map[string]bool{}),
		"an empty disk set must not condemn the whole repository")
	assert.Equal(t, []string{"live.go"}, idx.indexedFilesAbsentFromDisk(map[string]bool{"other.go": true}))
}
