package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// scopedNodesLight hands handlers nodes with Meta nil. The in-memory graph
// has no projection and returns canonical nodes with full Meta, so a
// handler wrongly converted to the light path passes every in-memory test
// and fails only against SQLite. These tests therefore run on a real
// SQLite store.
func lightProjectionStore(t *testing.T) *store_sqlite.Store {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "light.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	nodes := []*graph.Node{
		{
			ID: "repo-a/svc.go::Handler", Kind: graph.KindFunction, Name: "Handler",
			QualName: "svc.Handler", FilePath: "repo-a/svc.go", StartLine: 10, EndLine: 20,
			Language: "go", RepoPrefix: "repo-a", WorkspaceID: "ws-one",
			Meta: map[string]any{"signature": "func Handler()", "complexity": 7},
		},
		{
			ID: "repo-b/other.go::Worker", Kind: graph.KindFunction, Name: "Worker",
			QualName: "other.Worker", FilePath: "repo-b/other.go", StartLine: 1, EndLine: 5,
			Language: "go", RepoPrefix: "repo-b", WorkspaceID: "ws-two",
			Meta: map[string]any{"signature": "func Worker()"},
		},
	}
	store.AddBatch(nodes, nil)
	return store
}

// TestScopedNodesLightProjectionContract pins what a converted handler may
// rely on: the scope fields ScopeAllows needs are present, and Meta — with
// every promoted key that rides in it — is not.
func TestScopedNodesLightProjectionContract(t *testing.T) {
	store := lightProjectionStore(t)

	light := graph.AllNodesLight(store)
	require.Len(t, light, 2)

	byID := map[string]*graph.Node{}
	for _, n := range light {
		byID[n.ID] = n
	}
	got := byID["repo-a/svc.go::Handler"]
	require.NotNil(t, got)

	// Populated — these are what the 14 converted call sites read.
	require.Equal(t, graph.KindFunction, got.Kind)
	require.Equal(t, "Handler", got.Name)
	require.Equal(t, "svc.Handler", got.QualName)
	require.Equal(t, "repo-a/svc.go", got.FilePath)
	require.Equal(t, 10, got.StartLine)
	require.Equal(t, 20, got.EndLine)
	require.Equal(t, "go", got.Language)
	require.Equal(t, "repo-a", got.RepoPrefix)
	require.Equal(t, "ws-one", got.WorkspaceID)

	// Not populated. A handler reading any of these must keep using
	// scopedNodes; the light projection stops before the meta blob and
	// before the promoted columns it is rebuilt from.
	require.Nil(t, got.Meta, "the light projection must not decode meta")

	// The full scan still carries it, so the two are genuinely different.
	for _, n := range store.AllNodes() {
		if n.ID == "repo-a/svc.go::Handler" {
			require.NotNil(t, n.Meta, "the full scan still decodes meta")
			require.Equal(t, "func Handler()", n.Meta["signature"])
		}
	}
}

// TestScopedNodesLightAppliesSessionScope verifies the scoping filter works
// on projected nodes — ScopeAllows reads WorkspaceID / RepoPrefix /
// ProjectID, all of which the projection carries.
func TestScopedNodesLightAppliesSessionScope(t *testing.T) {
	store := lightProjectionStore(t)
	s := &Server{graph: store}

	ctx := context.Background()
	require.Len(t, s.scopedNodesLight(ctx), 2, "an unbound session sees everything")
	require.Len(t, s.scopedNodes(ctx), 2)

	scoped := withRepoAllow(ctx, map[string]bool{"repo-a": true})
	light := s.scopedNodesLight(scoped)
	require.Len(t, light, 1, "repo scope must narrow the projected set")
	require.Equal(t, "repo-a/svc.go::Handler", light[0].ID)

	// The two entry points agree on which nodes are in scope; they differ
	// only in how much of each node is materialized.
	full := s.scopedNodes(scoped)
	require.Len(t, full, 1)
	require.Equal(t, full[0].ID, light[0].ID)
	require.NotNil(t, full[0].Meta)
	require.Nil(t, light[0].Meta)
}
