package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/persistence"
)

// End-to-end for the anchor migration: a memory stored under the old
// unprefixed id convention must surface again after the rewrite, and the
// rewrite must not touch anchors it cannot prove.
//
// Without it the failure is silent — surface_memories returns Total: 0, which
// reads exactly like "nothing relevant to this task".

func anchorMigrationServer(t *testing.T, g *graph.Graph, repoKey string) (*Server, *persistence.SidecarStore) {
	t.Helper()
	sidecar, err := persistence.OpenSidecar(filepath.Join(t.TempDir(), "sidecar.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sidecar.Close() })

	srv := &Server{graph: g}
	srv.memories = newMemoryManagerFromSidecar(sidecar, repoKey, "")
	srv.notes = newNotesManagerFromSidecar(sidecar, repoKey, "")
	return srv, sidecar
}

func TestMigrateSymbolAnchorsRestoresSurfacing(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "gortex/internal/foo.go::Bar", Kind: graph.KindFunction, Name: "Bar",
		FilePath: "gortex/internal/foo.go", RepoPrefix: "gortex",
	})

	srv, sidecar := anchorMigrationServer(t, g, "repo-key")
	require.NoError(t, sidecar.UpsertMemory("repo-key", persistence.MemoryEntry{
		ID: "m1", Body: "Bar must hold the lock before mutating cache",
		SymbolIDs: []string{"internal/foo.go::Bar"}, // the old convention
	}))
	srv.memories.reload()

	resolve := func(id string) *graph.Node { return g.GetNode(id) }
	anchors := SurfaceOptions{SymbolIDs: []string{"gortex/internal/foo.go::Bar"}}

	// Before: the stored anchor cannot match, and the diagnostic says why.
	before := srv.memories.Surface(anchors, resolve)
	require.Zero(t, before.Total, "the pre-migration anchor must not match")
	require.Contains(t, before.AnchorDiagnostic, "different symbol-id convention")

	srv.MigrateSymbolAnchors(zap.NewNop())

	after := srv.memories.Surface(anchors, resolve)
	require.Equal(t, 1, after.Total, "the migrated anchor must surface again")
	assert.Equal(t, []string{"gortex/internal/foo.go::Bar"}, after.Memories[0].SymbolIDs)
	assert.Empty(t, after.AnchorDiagnostic)
}

func TestMigrateSymbolAnchorsLeavesAmbiguousAnchorsAlone(t *testing.T) {
	// The same repo-relative path exists in two tracked repos, so nothing
	// distinguishes which one the anchor meant.
	g := graph.New()
	for _, repo := range []string{"alpha", "beta"} {
		g.AddNode(&graph.Node{
			ID: repo + "/internal/foo.go::Bar", Kind: graph.KindFunction, Name: "Bar",
			FilePath: repo + "/internal/foo.go", RepoPrefix: repo,
		})
	}

	srv, sidecar := anchorMigrationServer(t, g, "repo-key")
	require.NoError(t, sidecar.UpsertMemory("repo-key", persistence.MemoryEntry{
		ID: "m1", Body: "b", SymbolIDs: []string{"internal/foo.go::Bar"},
	}))
	srv.memories.reload()

	srv.MigrateSymbolAnchors(zap.NewNop())

	rows, err := sidecar.LoadMemoriesRows("repo-key")
	require.NoError(t, err)
	assert.Equal(t, []string{"internal/foo.go::Bar"}, rows[0].SymbolIDs,
		"an anchor matching several repos must be left as stored, not guessed at")
}

func TestMigrateSymbolAnchorsNeverTouchesAResolvableAnchor(t *testing.T) {
	g := graph.New()
	// Both the bare and the prefixed id exist as nodes — the standalone
	// Indexer shape. The stored anchor already resolves, so it must not move.
	g.AddNode(&graph.Node{ID: "internal/foo.go::Bar", Kind: graph.KindFunction, Name: "Bar", FilePath: "internal/foo.go"})
	g.AddNode(&graph.Node{
		ID: "gortex/internal/foo.go::Bar", Kind: graph.KindFunction, Name: "Bar",
		FilePath: "gortex/internal/foo.go", RepoPrefix: "gortex",
	})

	srv, sidecar := anchorMigrationServer(t, g, "repo-key")
	require.NoError(t, sidecar.UpsertMemory("repo-key", persistence.MemoryEntry{
		ID: "m1", Body: "b", SymbolIDs: []string{"internal/foo.go::Bar"},
	}))
	srv.memories.reload()

	srv.MigrateSymbolAnchors(zap.NewNop())

	rows, err := sidecar.LoadMemoriesRows("repo-key")
	require.NoError(t, err)
	assert.Equal(t, []string{"internal/foo.go::Bar"}, rows[0].SymbolIDs,
		"an anchor that still names a live node is already correct and must not be rewritten")
}
