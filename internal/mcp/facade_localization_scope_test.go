package mcp

import (
	"context"
	"path/filepath"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func TestFacadeExplorePathLowersToExplicitRepoSelector(t *testing.T) {
	registry := newFacadeRegistry()
	registry.capture(mcpgo.NewTool(
		"explore",
		mcpgo.WithString("task", mcpgo.Required()),
		mcpgo.WithString("repo"),
	), func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultJSON(req.GetArguments())
	})
	server := &Server{facades: registry, localization: newLocalizationTerminalState()}
	path := "/tracked/worktrees/search-engine"
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"operation": "localize",
		"task":      "find the multi-root traversal implementation",
		"path":      path,
		"options":   map[string]any{"repo": "active-repo"},
	}
	result, err := server.handleFacade(context.Background(), "explore", req)
	require.NoError(t, err)
	arguments := unmarshalResult(t, result)
	require.Equal(t, path, arguments["repo"], "explicit path must win over an unrelated repo default")
	require.NotContains(t, arguments, "path", "legacy explore ignores path; it must be lowered to repo")
}

func TestResolveRepoPrefixAcceptsNestedTrackedPath(t *testing.T) {
	fixture := newSharedWorkspaceServer(t, true)
	nested := filepath.Join(fixture.repoB, "crates", "ignore", "src")
	require.Equal(t, "repo-b", fixture.srv.resolveRepoPrefix(nested))

	ctx := sessionCtx("nested-repo-path", fixture.repoA)
	req := makeReq("explore", map[string]any{"repo": nested})
	scope, errResult := fixture.srv.resolveScope(ctx, req, IntentLocate)
	require.Nil(t, errResult)
	require.Equal(t, map[string]bool{"repo-b": true}, scope.RepoAllow)

	unknown := filepath.Join(t.TempDir(), "not-tracked")
	_, errResult = fixture.srv.resolveScope(ctx, makeReq("explore", map[string]any{"repo": unknown}), IntentLocate)
	require.NotNil(t, errResult, "an explicit untracked path must fail instead of selecting the active repo")
}

func TestFacadeResolvedTargetShapesReachLegacyHandlers(t *testing.T) {
	g := graph.New()
	node := &graph.Node{
		ID: "repo/pkg/worker.go::Run", Name: "Run", QualName: "worker.Run",
		Kind: graph.KindFunction, FilePath: "repo/pkg/worker.go", RepoPrefix: "repo",
	}
	g.AddNode(node)
	registry := newFacadeRegistry()
	capture := func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultJSON(req.GetArguments())
	}
	registry.capture(mcpgo.NewTool("get_editing_context", mcpgo.WithString("path", mcpgo.Required())), capture)
	registry.capture(mcpgo.NewTool("explain_change_impact", mcpgo.WithString("ids", mcpgo.Required())), capture)
	registry.capture(mcpgo.NewTool("batch_symbols", mcpgo.WithString("ids", mcpgo.Required())), capture)
	server := &Server{
		engine: query.NewEngine(g), graph: g, facades: registry,
		localization: newLocalizationTerminalState(),
	}

	call := func(facade string, arguments map[string]any) map[string]any {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = arguments
		result, err := server.handleFacade(context.Background(), facade, req)
		require.NoError(t, err)
		require.False(t, result.IsError, "result: %#v", result)
		return unmarshalResult(t, result)
	}

	editing := call("read", map[string]any{
		"operation": "editing_context",
		"target":    map[string]any{"symbol": node.ID},
	})
	require.Equal(t, node.FilePath, editing["path"])
	require.NotContains(t, editing, "id")

	impact := call("change", map[string]any{
		"operation": "impact",
		"target":    map[string]any{"file": node.FilePath},
	})
	require.Equal(t, node.ID, impact["ids"])
	require.NotContains(t, impact, "path")

	batch := call("read", map[string]any{
		"operation": "source",
		"target":    map[string]any{"symbols": []any{node.ID}},
	})
	require.Equal(t, `["repo/pkg/worker.go::Run"]`, batch["ids"])
	require.Equal(t, true, batch["include_source"])
}

func TestFacadeResolvedTargetShapesAreAdvertised(t *testing.T) {
	registry := newFacadeRegistry()
	handler := func(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		return mcpgo.NewToolResultText("ok"), nil
	}
	registry.capture(mcpgo.NewTool("get_editing_context", mcpgo.WithString("path", mcpgo.Required())), handler)
	registry.capture(mcpgo.NewTool("explain_change_impact", mcpgo.WithString("ids", mcpgo.Required())), handler)
	server := &Server{facades: registry}

	targetProperties := func(domain, operation string) map[string]any {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = map[string]any{"domain": domain, "operation": operation, "detail": "schema"}
		result, err := server.handleCapabilities(context.Background(), req)
		require.NoError(t, err)
		payload := unmarshalResult(t, result)
		schema := payload["input_schema"].(map[string]any)
		properties := schema["properties"].(map[string]any)
		target := properties["target"].(map[string]any)
		return target["properties"].(map[string]any)
	}

	require.Contains(t, targetProperties("read", "editing_context"), "file")
	require.Contains(t, targetProperties("read", "editing_context"), "symbol")
	require.Contains(t, targetProperties("change", "impact"), "symbol")
	require.Contains(t, targetProperties("change", "impact"), "file")
}

func TestExploreRepeatedGenericConceptIsNeitherTerminalNorExact(t *testing.T) {
	targets := []exploreTarget{
		{node: &graph.Node{ID: "repo/a.go::walk", Name: "walk", QualName: "parser.walk", Kind: graph.KindFunction, FilePath: "repo/a.go"}, score: 2},
		{node: &graph.Node{ID: "repo/b.go::walk", Name: "walk", QualName: "tree.walk", Kind: graph.KindFunction, FilePath: "repo/b.go"}, score: 1},
	}
	task := "walk walk walk"
	require.True(t, exploreQueryIsConceptTask(task))
	require.False(t, exploreAnswerReady(task, targets))
	require.Empty(t, exploreLocalizationExactTarget(task, targets))

	anchored := &graph.Node{
		ID: "repo/pkg/walk.go::WalkBuilder.build", Name: "build", QualName: "WalkBuilder.build",
		Kind: graph.KindMethod, FilePath: "repo/pkg/walk.go",
	}
	anchorTask := "repo/pkg/walk.go::WalkBuilder.build"
	anchorTargets := []exploreTarget{{node: anchored, score: 1}}
	require.True(t, exploreAnswerReady(anchorTask, anchorTargets))
	require.Equal(t, anchored.ID, exploreLocalizationExactTarget(anchorTask, anchorTargets))
}
