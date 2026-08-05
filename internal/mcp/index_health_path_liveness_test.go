package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A clean workspace must say so explicitly. The block is the evidence that
// health_score 100 was checked against the filesystem rather than merely
// inferred from what the daemon happened to be tracking.
func TestIndexHealth_PathLivenessCleanWorkspace(t *testing.T) {
	srv, _ := setupTestServer(t)

	payload := srv.buildIndexHealthPayload()
	require.NotNil(t, payload)

	liveness, ok := payload["path_liveness"].(map[string]any)
	require.True(t, ok, "index_health must report path liveness")
	assert.Equal(t, true, liveness["clean"])
	assert.Equal(t, 0, liveness["orphan_files"])
	assert.Positive(t, liveness["checked"])
	assert.NotContains(t, payload, "recommendation")
}

// The issue: a deleted subtree's nodes survive in the graph, keep answering
// searches, and every existing signal stays green because nothing tracks them
// any more. index_health has to be the thing that says otherwise.
func TestIndexHealth_ReportsOrphanedFileNodes(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.graph.AddBatch([]*graph.Node{
		{ID: "tmp/agent-skills/gamma.go", Kind: graph.KindFile, Name: "gamma.go", FilePath: "tmp/agent-skills/gamma.go"},
		{ID: "tmp/agent-skills/delta.go", Kind: graph.KindFile, Name: "delta.go", FilePath: "tmp/agent-skills/delta.go"},
	}, nil)

	payload := srv.buildIndexHealthPayload()
	require.NotNil(t, payload)

	liveness, ok := payload["path_liveness"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, false, liveness["clean"])
	assert.Equal(t, 2, liveness["orphan_files"])
	assert.NotEmpty(t, liveness["orphan_samples"])

	score, ok := payload["health_score"].(float64)
	require.True(t, ok)
	assert.Less(t, score, 100.0, "a graph holding vanished files must not report 100%")

	recommendation, ok := payload["recommendation"].(string)
	require.True(t, ok, "orphans must come with remediation")
	assert.Contains(t, recommendation, "no longer exist on disk")
	assert.Contains(t, recommendation, "reindex_repository")
}

// Synthetic attribution paths name no file on disk by design. Counting them as
// orphans would put every workspace with external references permanently below
// 100% — the false-positive that makes a health signal worth ignoring.
func TestIndexHealth_PathLivenessIgnoresSyntheticPaths(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.graph.AddBatch([]*graph.Node{
		{ID: "external::fmt", Kind: graph.KindFile, Name: "fmt", FilePath: "external::fmt"},
		{ID: "external-call::os.Exit", Kind: graph.KindFile, Name: "os.Exit", FilePath: "external-call::os.Exit"},
	}, nil)

	payload := srv.buildIndexHealthPayload()
	liveness, ok := payload["path_liveness"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, liveness["clean"])
	assert.Equal(t, 0, liveness["orphan_files"])
}

// A stat that fails for a reason other than absence is not evidence of a
// deletion. Reporting it as one would turn a permissions problem into a
// spurious "rebuild your graph".
func TestIndexHealth_PathLivenessAbstainsWhenRootUnknown(t *testing.T) {
	srv, _ := setupTestServer(t)

	srv.graph.AddBatch([]*graph.Node{
		{ID: "ghost/lost.go", Kind: graph.KindFile, Name: "lost.go", FilePath: "ghost/lost.go", RepoPrefix: "ghost"},
	}, nil)

	payload := srv.buildIndexHealthPayload()
	liveness, ok := payload["path_liveness"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, liveness["clean"], "a prefix with no resolvable root must not be condemned")
	assert.Equal(t, 1, liveness["unresolvable"])
}

// The stale-file signal and the liveness signal answer different questions:
// one covers files the daemon still tracks, the other covers files it stopped
// tracking. The bug was believing the first ruled out the second.
func TestIndexHealth_OrphansSurviveAZeroStaleReport(t *testing.T) {
	srv, dir := setupTestServer(t)

	gone := filepath.Join(dir, "tmpcopy", "gamma.go")
	require.NoError(t, os.MkdirAll(filepath.Dir(gone), 0o755))
	require.NoError(t, os.WriteFile(gone, []byte("package tmpcopy\n"), 0o644))
	require.NoError(t, srv.indexer.IndexFile(gone))
	require.NoError(t, os.RemoveAll(filepath.Join(dir, "tmpcopy")))

	// Exactly the state the report describes: the deletion left no mtime
	// record behind, so staleness has nothing to report on.
	srv.indexer.SetFileMtimes(map[string]int64{})

	payload := srv.buildIndexHealthPayload()
	require.NotContains(t, payload, "stale_files")

	liveness, ok := payload["path_liveness"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, 1, liveness["orphan_files"])
	assert.Equal(t, false, liveness["clean"])
}
