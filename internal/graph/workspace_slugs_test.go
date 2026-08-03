package graph

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGraphBackfillWorkspaceSlugsReportsOnlyEffectiveBoundaryChanges(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{
		{ID: "default/a.go::A", Kind: KindFunction, RepoPrefix: "default"},
		{ID: "member/b.go::B", Kind: KindFunction, RepoPrefix: "member"},
		{ID: "project/c.go::C", Kind: KindFunction, RepoPrefix: "project", WorkspaceID: "existing"},
	}, nil)

	result := g.BackfillWorkspaceSlugsWithImpact([]WorkspaceSlug{
		{RepoPrefix: "default", Workspace: "default", Project: "default-project"},
		{RepoPrefix: "member", Workspace: "shared", Project: "member-project"},
		{RepoPrefix: "project", Workspace: "ignored", Project: "project-id"},
	})
	require.Equal(t, WorkspaceSlugBackfillResult{Changed: 3, ResolutionAffected: 1}, result)

	defaultNode := g.GetNode("default/a.go::A")
	require.Equal(t, "default", defaultNode.WorkspaceID)
	require.Equal(t, "default-project", defaultNode.ProjectID)
	memberNode := g.GetNode("member/b.go::B")
	require.Equal(t, "shared", memberNode.WorkspaceID)
	require.Equal(t, "member-project", memberNode.ProjectID)
	projectNode := g.GetNode("project/c.go::C")
	require.Equal(t, "existing", projectNode.WorkspaceID)
	require.Equal(t, "project-id", projectNode.ProjectID)

	require.Equal(t, WorkspaceSlugBackfillResult{}, g.BackfillWorkspaceSlugsWithImpact([]WorkspaceSlug{
		{RepoPrefix: "default", Workspace: "default", Project: "default-project"},
		{RepoPrefix: "member", Workspace: "shared", Project: "member-project"},
		{RepoPrefix: "project", Workspace: "ignored", Project: "project-id"},
	}))
}

func TestBackfillWorkspaceSlugsWithImpactIgnoresBuiltinResolutionImpact(t *testing.T) {
	g := New()
	g.AddBatch([]*Node{
		{ID: "repo::builtin::go::type::error", Kind: KindBuiltin, Name: "error", RepoPrefix: "repo"},
		{ID: "repo::function", Kind: KindFunction, Name: "function", RepoPrefix: "repo"},
	}, nil)

	result := g.BackfillWorkspaceSlugsWithImpact([]WorkspaceSlug{{
		RepoPrefix: "repo",
		Workspace:  "shared",
		Project:    "project",
	}})
	require.Equal(t, WorkspaceSlugBackfillResult{Changed: 2, ResolutionAffected: 1}, result)
	for _, id := range []string{"repo::builtin::go::type::error", "repo::function"} {
		node := g.GetNode(id)
		require.NotNil(t, node)
		require.Equal(t, "shared", node.WorkspaceID)
		require.Equal(t, "project", node.ProjectID)
	}
}
