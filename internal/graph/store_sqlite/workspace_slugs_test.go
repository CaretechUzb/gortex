package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestBackfillWorkspaceSlugsIsDurableAndIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	store.AddBatch([]*graph.Node{
		{ID: "repoA/a.go::A", Kind: graph.KindFunction, RepoPrefix: "repoA", Meta: map[string]any{"opaque": "keep"}},
		{ID: "repoA/b.go::B", Kind: graph.KindFunction, RepoPrefix: "repoA"},
		{ID: "repoB/c.go::C", Kind: graph.KindFunction, RepoPrefix: "repoB", WorkspaceID: "existing"},
		{ID: "repoC/d.go::D", Kind: graph.KindFunction, RepoPrefix: "repoC"},
	}, nil)

	rows := []graph.WorkspaceSlug{
		{RepoPrefix: "repoA", Workspace: "workspace-a", Project: "project-a"},
		{RepoPrefix: "repoB", Workspace: "workspace-b", Project: "project-b"},
		{RepoPrefix: "repoC", Workspace: "repoC", Project: "project-c"},
	}
	beforeRevision := store.AnalysisMutationRevision()
	require.Equal(t, graph.WorkspaceSlugBackfillResult{Changed: 4, ResolutionAffected: 2},
		store.BackfillWorkspaceSlugsWithImpact(rows))
	require.Equal(t, beforeRevision+1, store.AnalysisMutationRevision())

	a := store.GetNode("repoA/a.go::A")
	require.NotNil(t, a)
	require.Equal(t, "workspace-a", a.WorkspaceID)
	require.Equal(t, "project-a", a.ProjectID)
	require.Equal(t, "keep", a.Meta["opaque"])
	c := store.GetNode("repoB/c.go::C")
	require.NotNil(t, c)
	require.Equal(t, "existing", c.WorkspaceID, "non-empty workspace must be preserved")
	require.Equal(t, "project-b", c.ProjectID)
	d := store.GetNode("repoC/d.go::D")
	require.NotNil(t, d)
	require.Equal(t, "repoC", d.WorkspaceID, "RepoPrefix-equivalent workspace fill is resolution-neutral")
	require.Equal(t, "project-c", d.ProjectID)

	beforeRevision = store.AnalysisMutationRevision()
	require.Equal(t, graph.WorkspaceSlugBackfillResult{}, store.BackfillWorkspaceSlugsWithImpact(rows))
	require.Equal(t, beforeRevision, store.AnalysisMutationRevision())
	require.NoError(t, store.Close())

	store, err = Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	a = store.GetNode("repoA/a.go::A")
	require.NotNil(t, a)
	require.Equal(t, "workspace-a", a.WorkspaceID)
	require.Equal(t, "project-a", a.ProjectID)
	require.Equal(t, "keep", a.Meta["opaque"])
}

func TestBackfillWorkspaceSlugsWithImpactIgnoresBuiltinResolutionImpact(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	store.AddBatch([]*graph.Node{
		{ID: "repo::builtin::go::type::error", Kind: graph.KindBuiltin, Name: "error", RepoPrefix: "repo"},
		{ID: "repo::function", Kind: graph.KindFunction, Name: "function", RepoPrefix: "repo"},
	}, nil)

	result := store.BackfillWorkspaceSlugsWithImpact([]graph.WorkspaceSlug{{
		RepoPrefix: "repo",
		Workspace:  "shared",
		Project:    "project",
	}})
	require.Equal(t, graph.WorkspaceSlugBackfillResult{Changed: 2, ResolutionAffected: 1}, result)
	for _, id := range []string{"repo::builtin::go::type::error", "repo::function"} {
		node := store.GetNode(id)
		require.NotNil(t, node)
		require.Equal(t, "shared", node.WorkspaceID)
		require.Equal(t, "project", node.ProjectID)
	}
}
