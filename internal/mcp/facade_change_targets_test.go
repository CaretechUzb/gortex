package mcp

import (
	"context"
	"sync/atomic"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func TestFacadeChangeTargetsNormalizeScalarArrayAndTargetShapes(t *testing.T) {
	server, calls := newFacadeChangeCaptureServer(t)
	ids := []string{"repo/a.go::A", "repo/b.go::B"}
	shapes := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "scalar ids", args: map[string]any{"options": map[string]any{"ids": ids[0] + "," + ids[1]}}, want: ids[0] + "," + ids[1]},
		{name: "array ids", args: map[string]any{"options": map[string]any{"ids": []any{ids[0], ids[1]}}}, want: ids[0] + "," + ids[1]},
		{name: "target symbol", args: map[string]any{"target": map[string]any{"symbol": ids[0]}}, want: ids[0]},
		{name: "target symbols", args: map[string]any{"target": map[string]any{"symbols": []any{ids[0], ids[1]}}}, want: ids[0] + "," + ids[1]},
		{name: "source symbols compatibility", args: map[string]any{"source": map[string]any{"symbols": []any{ids[0], ids[1]}}}, want: ids[0] + "," + ids[1]},
	}

	for _, operation := range []string{"tests", "guards", "edit_plan"} {
		for _, shape := range shapes {
			t.Run(operation+"/"+shape.name, func(t *testing.T) {
				args := cloneFacadeTestMap(shape.args)
				args["operation"] = operation
				payload := callFacadeChangeCapture(t, server, args)
				require.Equal(t, shape.want, payload["ids"])
				require.NotContains(t, payload, "id")
				require.NotContains(t, payload, "symbols")
			})
		}
	}
	require.Equal(t, int64(len(shapes)*3), calls.Load())
}

func TestFacadeChangeContractHonorsExplicitSymbolTargets(t *testing.T) {
	server, calls := newFacadeChangeCaptureServer(t)
	ids := []string{"repo/a.go::A", "repo/b.go::B"}
	shapes := []struct {
		name string
		args map[string]any
		want string
	}{
		{name: "scalar ids", args: map[string]any{"options": map[string]any{"ids": ids[0] + "," + ids[1]}}, want: ids[0] + "," + ids[1]},
		{name: "array ids", args: map[string]any{"options": map[string]any{"ids": []any{ids[0], ids[1]}}}, want: ids[0] + "," + ids[1]},
		{name: "target symbol", args: map[string]any{"target": map[string]any{"symbol": ids[0]}}, want: ids[0]},
		{name: "target symbols", args: map[string]any{"target": map[string]any{"symbols": []any{ids[0], ids[1]}}}, want: ids[0] + "," + ids[1]},
		{name: "source symbols compatibility", args: map[string]any{"source": map[string]any{"source": "symbols", "symbols": []any{ids[0], ids[1]}}}, want: ids[0] + "," + ids[1]},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			args := cloneFacadeTestMap(shape.args)
			args["operation"] = "contract"
			payload := callFacadeChangeCapture(t, server, args)
			require.Equal(t, "symbols", payload["source"])
			require.Equal(t, shape.want, payload["symbols"])
			require.NotContains(t, payload, "id")
			require.NotContains(t, payload, "ids")
		})
	}
	// Every explicit target reached the symbols path. None silently lowered as
	// source=diff, which would analyze the entire dirty worktree.
	require.Equal(t, int64(len(shapes)), calls.Load())
}

func TestFacadeChangeTargetsRejectEmptyConflictAndImplicitDiff(t *testing.T) {
	server, calls := newFacadeChangeCaptureServer(t)

	assertError := func(args map[string]any, contains string) {
		t.Helper()
		req := mcpgo.CallToolRequest{}
		req.Params.Arguments = args
		result, err := server.handleFacade(context.Background(), "change", req)
		require.NoError(t, err)
		require.True(t, result.IsError, "result: %#v", result)
		require.Contains(t, toolResultText(result), contains)
	}

	assertError(map[string]any{
		"operation": "tests",
		"options":   map[string]any{"ids": []any{}},
	}, "must not be empty")
	assertError(map[string]any{
		"operation": "guards",
		"target":    map[string]any{"symbol": "repo/a.go::A"},
		"options":   map[string]any{"ids": "repo/b.go::B"},
	}, "conflicting symbol selectors")
	assertError(map[string]any{
		"operation": "contract",
	}, "requires target.symbol/target.symbols or an explicit non-symbol source")
	assertError(map[string]any{
		"operation": "contract",
		"target":    map[string]any{"symbol": "repo/a.go::A"},
		"source":    map[string]any{"source": "diff"},
	}, "conflict with source=diff")
	require.Zero(t, calls.Load(), "invalid requests must not reach a legacy handler")

	payload := callFacadeChangeCapture(t, server, map[string]any{
		"operation": "contract",
		"source":    map[string]any{"source": "diff", "scope": "unstaged"},
	})
	require.Equal(t, "diff", payload["source"])
	require.Equal(t, "unstaged", payload["scope"])
	require.NotContains(t, payload, "symbols")
	require.Equal(t, int64(1), calls.Load())
}

func TestFacadeChangeTargetShapesAreAdvertisedOnce(t *testing.T) {
	server, _ := newFacadeChangeCaptureServer(t)
	for _, operation := range []string{"tests", "guards", "edit_plan", "contract"} {
		t.Run(operation, func(t *testing.T) {
			req := mcpgo.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"domain": "change", "operation": operation, "detail": "schema",
			}
			result, err := server.handleCapabilities(context.Background(), req)
			require.NoError(t, err)
			payload := unmarshalResult(t, result)
			schema := payload["input_schema"].(map[string]any)
			properties := schema["properties"].(map[string]any)
			target := properties["target"].(map[string]any)
			targetProperties := target["properties"].(map[string]any)
			require.Contains(t, targetProperties, "symbol")
			require.Contains(t, targetProperties, "symbols")
			if source, ok := properties["source"].(map[string]any); ok {
				sourceProperties, _ := source["properties"].(map[string]any)
				require.NotContains(t, sourceProperties, "symbols", "symbol selectors must be advertised only under target")
			}
			if options, ok := properties["options"].(map[string]any); ok {
				optionProperties, _ := options["properties"].(map[string]any)
				require.NotContains(t, optionProperties, "ids", "compatibility aliases must not duplicate the canonical target schema")
			}

			requestShape := payload["request_shape"].(map[string]any)
			require.Contains(t, requestShape, "target")
		})
	}
}

func newFacadeChangeCaptureServer(t *testing.T) (*Server, *atomic.Int64) {
	t.Helper()
	registry := newFacadeRegistry()
	calls := &atomic.Int64{}
	capture := func(_ context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		calls.Add(1)
		return mcpgo.NewToolResultJSON(req.GetArguments())
	}
	registry.capture(mcpgo.NewTool("get_test_targets", mcpgo.WithString("ids", mcpgo.Required())), capture)
	registry.capture(mcpgo.NewTool("check_guards", mcpgo.WithString("ids", mcpgo.Required())), capture)
	registry.capture(mcpgo.NewTool("get_edit_plan", mcpgo.WithString("ids", mcpgo.Required())), capture)
	registry.capture(mcpgo.NewTool("change_contract",
		mcpgo.WithString("source"),
		mcpgo.WithString("symbols"),
		mcpgo.WithString("scope"),
		mcpgo.WithBoolean("ack"),
	), capture)

	g := graph.New()
	return &Server{
		engine: query.NewEngine(g), graph: g, facades: registry,
		localization: newLocalizationTerminalState(),
	}, calls
}

func callFacadeChangeCapture(t *testing.T, server *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcpgo.CallToolRequest{}
	req.Params.Arguments = args
	result, err := server.handleFacade(context.Background(), "change", req)
	require.NoError(t, err)
	require.False(t, result.IsError, "result: %s", toolResultText(result))
	return unmarshalResult(t, result)
}

func cloneFacadeTestMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		if nested, ok := value.(map[string]any); ok {
			out[key] = cloneFacadeTestMap(nested)
			continue
		}
		out[key] = value
	}
	return out
}
