package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

func TestRunPreEnrichResolveBackfillsBeforeFullCross(t *testing.T) {
	mi, store, call := newPreEnrichSlugFixture(t, false)
	hooks := 0

	err := mi.RunPreEnrichResolve(context.Background(), nil, func() {
		hooks++
		call.To = "unresolved::Serve"
	})

	require.NoError(t, err)
	require.Equal(t, 1, hooks)
	require.Equal(t, "b/b.go::Serve", call.To)
	require.Equal(t, "shared", store.GetNode("a/a.go::Call").WorkspaceID)
	require.Equal(t, "shared", store.GetNode("b/b.go::Serve").WorkspaceID)
}

func TestRunPreEnrichResolveFilesEscalatesCrossAfterLegacyNodeStamp(t *testing.T) {
	mi, _, call := newPreEnrichSlugFixture(t, false)
	hooks := 0

	err := mi.RunPreEnrichResolveFiles(context.Background(), []string{"c/c.go"}, func() {
		hooks++
		call.To = "unresolved::Serve"
	})

	require.NoError(t, err)
	require.Equal(t, 1, hooks)
	require.Equal(t, "b/b.go::Serve", call.To, "full migration catch-up missed an edge outside the exact file frontier")
}

func TestRunPreEnrichResolveFilesKeepsStampedGraphFileScoped(t *testing.T) {
	mi, _, call := newPreEnrichSlugFixture(t, true)
	hooks := 0

	err := mi.RunPreEnrichResolveFiles(context.Background(), []string{"c/c.go"}, func() {
		hooks++
		call.To = "unresolved::Serve"
	})

	require.NoError(t, err)
	require.Equal(t, 1, hooks)
	require.Equal(t, "unresolved::Serve", call.To, "exact cross pass leaked outside its file frontier")
}

func TestRunPreEnrichResolveFilesCanceledBeforeComputePreservesHookBoundary(t *testing.T) {
	mi, store, call := newPreEnrichSlugFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	hooks := 0

	err := mi.RunPreEnrichResolveFiles(ctx, []string{"c/c.go"}, func() { hooks++ })

	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, hooks)
	require.Equal(t, "unresolved::Serve", call.To)
	require.Empty(t, store.GetNode("a/a.go::Call").WorkspaceID)
}

func newPreEnrichSlugFixture(t *testing.T, stamped bool) (*MultiIndexer, *graph.Graph, *graph.Edge) {
	t.Helper()
	roots := map[string]string{
		"a": t.TempDir(),
		"b": t.TempDir(),
		"c": t.TempDir(),
	}
	globalPath := filepath.Join(t.TempDir(), "config.yaml")
	global := &config.GlobalConfig{Repos: []config.RepoEntry{
		{Path: roots["a"], Name: "a", Workspace: "shared"},
		{Path: roots["b"], Name: "b", Workspace: "shared"},
		{Path: roots["c"], Name: "c", Workspace: "shared"},
	}}
	global.SetConfigPath(globalPath)
	require.NoError(t, global.Save())
	configMgr, err := config.NewConfigManager(globalPath)
	require.NoError(t, err)

	workspaceID := ""
	if stamped {
		workspaceID = "shared"
	}
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "a/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a/a.go", Language: "go", RepoPrefix: "a", WorkspaceID: workspaceID},
		{ID: "a/a.go::Call", Kind: graph.KindFunction, Name: "Call", FilePath: "a/a.go", Language: "go", RepoPrefix: "a", WorkspaceID: workspaceID},
		{ID: "b/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b/b.go", Language: "go", RepoPrefix: "b", WorkspaceID: workspaceID},
		{ID: "b/b.go::Serve", Kind: graph.KindFunction, Name: "Serve", FilePath: "b/b.go", Language: "go", RepoPrefix: "b", WorkspaceID: workspaceID},
		{ID: "c/c.go", Kind: graph.KindFile, Name: "c.go", FilePath: "c/c.go", Language: "go", RepoPrefix: "c", WorkspaceID: workspaceID},
		{ID: "c/c.go::Changed", Kind: graph.KindFunction, Name: "Changed", FilePath: "c/c.go", Language: "go", RepoPrefix: "c", WorkspaceID: workspaceID},
	}, nil)
	base.AddEdge(&graph.Edge{From: "a/a.go", To: "b/b.go", Kind: graph.EdgeImports, FilePath: "a/a.go", Line: 1})
	call := &graph.Edge{From: "a/a.go::Call", To: "unresolved::Serve", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 3}
	base.AddEdge(call)
	store := base
	mi := NewMultiIndexer(store, newTestRegistry(), search.NewNull(), configMgr, zap.NewNop())
	mi.repos = map[string]*RepoMetadata{
		"a": {RepoPrefix: "a", RootPath: roots["a"]},
		"b": {RepoPrefix: "b", RootPath: roots["b"]},
		"c": {RepoPrefix: "c", RootPath: roots["c"]},
	}
	if stamped {
		// AddBatch may materialize repository-owned builtins. Stamp the complete
		// fixture so this case represents a graph after its one-time migration.
		mi.BackfillWorkspaceSlugs()
	}
	return mi, store, call
}
