package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

// facadeFrameCaller drives real MCP frames against the registered tool
// surface, so every layer between the wire and the legacy handler runs —
// including the legacy-facade compatibility wrapper that decides whether a
// call is lowered through the public dispatcher. Handler-level helpers such
// as findAndCallHandler bypass that wrapper and cannot see this class of bug.
func facadeFrameCaller(t *testing.T, srv *Server, ctx context.Context) func(int, string, map[string]any) *mcpgo.CallToolResult {
	t.Helper()
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"integration-harness","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))
	return func(id int, name string, arguments map[string]any) *mcpgo.CallToolResult {
		t.Helper()
		frame, err := json.Marshal(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  "tools/call",
			"params":  map[string]any{"name": name, "arguments": arguments},
		})
		require.NoError(t, err)
		reply := srv.MCPServer().HandleMessage(ctx, frame)
		require.NotNil(t, reply)
		raw, err := json.Marshal(reply)
		require.NoError(t, err)
		var envelope struct {
			Error  any                   `json:"error"`
			Result *mcpgo.CallToolResult `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw, &envelope))
		require.Nil(t, envelope.Error)
		require.NotNil(t, envelope.Result)
		return envelope.Result
	}
}

func fixtureSymbolID(t *testing.T, srv *Server, ctx context.Context, name string) string {
	t.Helper()
	for _, node := range srv.engineFor(ctx).FindSymbols(name) {
		if node != nil && node.Name == name {
			return node.ID
		}
	}
	t.Fatalf("fixture must index a symbol named %q", name)
	return ""
}

// TestAnalyzeTargetReachesHandlerOnEverySurface pins the promise the analyze
// schema and capabilities both advertise: target:{symbol} scopes the ranking
// to that symbol's blast radius. The compact target vocabulary must be lowered
// for every tool preset, not only inside a facade-v1 session — an agent that
// negotiated `core` gets the same tool description and the same capabilities
// answer, so a silently dropped target hands it a repo-wide hotspot list that
// is indistinguishable from a correct blast radius.
func TestAnalyzeTargetReachesHandlerOnEverySurface(t *testing.T) {
	for _, surface := range []struct {
		name string
		spec string
		mode string
	}{
		{name: "core_defer", spec: "core", mode: "defer"},
		{name: "full_hide", spec: "full", mode: "hide"},
		{name: "facade_v1", spec: FacadeSurfaceVersion, mode: "hide"},
	} {
		t.Run(surface.name, func(t *testing.T) {
			srv, _ := setupTestServer(t)
			sessionID := "analyze_target_" + surface.name
			ctx := WithSessionID(context.Background(), sessionID)
			call := facadeFrameCaller(t, srv, ctx)
			srv.NoteSessionToolPolicy(sessionID, surface.spec, surface.mode)
			require.Equal(t, surface.spec, srv.effectiveSessionPolicy(ctx).preset,
				"the harness must exercise the surface under test")

			helperID := fixtureSymbolID(t, srv, ctx, "helper")

			result := call(2, "analyze", map[string]any{
				"kind":   "impact",
				"target": map[string]any{"symbol": helperID},
				"output": map[string]any{"format": "json"},
			})
			require.False(t, result.IsError, toolResultText(result))
			payload := unmarshalResult(t, result)
			require.Equal(t, "target_closure", payload["scope"],
				"target:{symbol} must rank the target's closure, not the repo")
			target, ok := payload["target"].(map[string]any)
			require.True(t, ok, "a target-scoped impact must echo its target: %s", toolResultText(result))
			require.Equal(t, helperID, target["symbol"])
			require.Contains(t, toolResultText(result), helperID,
				"the target symbol must appear in its own blast radius")

			// The decisive negative control from the report: an unresolvable
			// target must fail closed. A repo-wide ranking here means the
			// selector never reached the handler.
			missing := call(3, "analyze", map[string]any{
				"kind":   "impact",
				"target": map[string]any{"symbol": "GORTEX_NEGATIVE_CONTROL_NO_SUCH_SYMBOL_9f68c2a1"},
				"output": map[string]any{"format": "json"},
			})
			require.True(t, missing.IsError, "an unresolvable impact target must be an error, got: %s", toolResultText(missing))
			require.Contains(t, toolResultText(missing), ErrCodeSymbolNotFound)
		})
	}
}

// TestAnalyzeUnsupportedTargetFailsClosed pins the other half of the contract:
// a kind with nothing to rank must refuse a target rather than answer the
// repo-wide question the caller did not ask.
func TestAnalyzeUnsupportedTargetFailsClosed(t *testing.T) {
	srv, _ := setupTestServer(t)
	ctx := WithSessionID(context.Background(), "analyze_unsupported_target")
	call := facadeFrameCaller(t, srv, ctx)
	helperID := fixtureSymbolID(t, srv, ctx, "helper")

	for i, kind := range []string{"routes", "hotspots", "dead_code"} {
		t.Run(kind, func(t *testing.T) {
			result := call(100+i, "analyze", map[string]any{
				"kind":   kind,
				"target": map[string]any{"symbol": helperID},
				"output": map[string]any{"format": "json"},
			})
			require.True(t, result.IsError,
				"analyze(kind=%s) must refuse a target it cannot rank, got: %s", kind, toolResultText(result))
			text := toolResultText(result)
			require.Contains(t, text, "unsupported_target")
			require.Contains(t, text, "impact", "the refusal should point at the kinds that do accept a target")
		})
	}

	// def_use consumes a symbol target but not a file one.
	fileTargeted := call(120, "analyze", map[string]any{
		"kind":   "def_use",
		"target": map[string]any{"file": "main.go"},
		"output": map[string]any{"format": "json"},
	})
	require.True(t, fileTargeted.IsError, "def_use ranks symbols, not files: %s", toolResultText(fileTargeted))
	require.Contains(t, toolResultText(fileTargeted), "unsupported_target")

	// A missing selector must speak the vocabulary the caller's schema uses.
	empty := call(121, "analyze", map[string]any{
		"kind":   "def_use",
		"output": map[string]any{"format": "json"},
	})
	require.True(t, empty.IsError)
	require.Contains(t, toolResultText(empty), "target:{symbol",
		"the error must name the public selector, not only the legacy field")
}

// TestAnalyzeAdvertisedTargetMatchesAcceptedTarget is the anti-drift gate the
// original report asked for: capabilities is the documentation callers build
// against, so a kind whose published request shape carries a target must
// accept one, and a kind whose shape omits it must refuse one. Neither
// direction may be discovered at runtime by getting a confident wrong answer.
func TestAnalyzeAdvertisedTargetMatchesAcceptedTarget(t *testing.T) {
	srv, _ := setupTestServer(t)
	ctx := WithSessionID(context.Background(), "analyze_target_parity")
	call := facadeFrameCaller(t, srv, ctx)
	helperID := fixtureSymbolID(t, srv, ctx, "helper")

	id := 200
	checked := 0
	advertised := 0
	for _, spec := range srv.capabilityOperations("analyze") {
		if spec.Legacy != "analyze" || spec.Operation == "help" {
			continue
		}
		capability := srv.facadeCapability(spec, true)
		shape, _ := capability["request_shape"].(map[string]any)
		arguments, _ := shape["arguments"].(map[string]any)
		_, publishesTarget := arguments["target"]

		id++
		result := call(id, "analyze", map[string]any{
			"kind":    spec.Operation,
			"target":  map[string]any{"symbol": helperID},
			"options": map[string]any{"limit": 1},
			"output":  map[string]any{"format": "json"},
		})
		refused := result.IsError && strings.Contains(toolResultText(result), "unsupported_target")
		checked++
		if publishesTarget {
			advertised++
			require.Falsef(t, refused,
				"capabilities publishes target for analyze(kind=%s) but the dispatcher refuses it", spec.Operation)
			continue
		}
		require.Truef(t, refused,
			"analyze(kind=%s) does not publish a target but accepted one — it would be silently ignored: %s",
			spec.Operation, toolResultText(result))
	}
	require.Greater(t, checked, 20, "the analyze catalogue must be exercised, not skipped")
	require.GreaterOrEqual(t, advertised, 2, "impact and def_use both publish a target")
}
