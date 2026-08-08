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

	// A malformed selector is refused, not quietly ignored: a scalar where the
	// envelope is expected used to slip past every lowering layer and land as a
	// repo-wide ranking.
	scalar := call(122, "analyze", map[string]any{
		"kind":   "impact",
		"target": helperID,
		"output": map[string]any{"format": "json"},
	})
	require.True(t, scalar.IsError, "a scalar target must be refused: %s", toolResultText(scalar))
	require.Contains(t, toolResultText(scalar), "target must be an object")

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
// original report asked for, and it is deliberately symmetric: capabilities is
// the documentation callers build against, so a kind whose published request
// shape carries a target must accept one, and a kind whose shape omits it must
// refuse one. Neither direction may be discovered at runtime by getting a
// confident wrong answer — the kind that publishes nothing (routes, hotspots,
// dead_code, …) is the direction the original report tripped over.
//
// Every non-admin analyze kind is covered, including the ones that dispatch to
// a legacy tool other than the analyze dispatcher; those were the blind spot
// where a target could still be accepted without ever being advertised.
func TestAnalyzeAdvertisedTargetMatchesAcceptedTarget(t *testing.T) {
	srv, _ := setupTestServer(t)
	ctx := WithSessionID(context.Background(), "analyze_target_parity")
	call := facadeFrameCaller(t, srv, ctx)
	helperID := fixtureSymbolID(t, srv, ctx, "helper")

	id := 200
	checked := 0
	advertised := 0
	aliased := 0
	for _, spec := range srv.capabilityOperations("analyze") {
		if spec.Operation == "help" {
			continue
		}
		if spec.Legacy != "analyze" {
			aliased++
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
	require.Greater(t, aliased, 10,
		"the kinds dispatching to another legacy tool must be covered, not filtered out")
}

// TestInertSelectorsFailClosedAcrossFacades extends the analyze contract to the
// rest of the compact surface: a selector the selected operation cannot consume
// is refused, not accepted and dropped. Each case below was verified to reach a
// legacy handler that has no field for the value.
func TestInertSelectorsFailClosedAcrossFacades(t *testing.T) {
	srv, _ := setupTestServer(t)
	sessionID := "inert_selectors"
	ctx := WithSessionID(context.Background(), sessionID)
	call := facadeFrameCaller(t, srv, ctx)
	srv.NoteSessionToolPolicy(sessionID, FacadeSurfaceVersion, "hide")
	helperID := fixtureSymbolID(t, srv, ctx, "helper")

	cases := []struct {
		name      string
		tool      string
		arguments map[string]any
	}{
		{
			// analyze kinds that dispatch to a different legacy tool were the
			// blind spot of the first fix: find_clones has no id field, so the
			// selector was lowered into nothing.
			name: "analyze_kind_on_another_legacy_tool",
			tool: "analyze",
			arguments: map[string]any{
				"kind": "clones", "target": map[string]any{"symbol": helperID},
			},
		},
		{
			// `to` is advertised on every relations operation but only the
			// trace flow/path/taint operations lower it into a sink field.
			name: "relations_destination_selector",
			tool: "relations",
			arguments: map[string]any{
				"operation": "usages",
				"target":    map[string]any{"symbol": helperID},
				"to":        map[string]any{"symbol": helperID},
			},
		},
		{
			name: "change_detect_target",
			tool: "change",
			arguments: map[string]any{
				"operation": "detect", "target": map[string]any{"file": "main.go"},
			},
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.arguments["output"] = map[string]any{"format": "json"}
			result := call(300+i, tc.tool, tc.arguments)
			require.True(t, result.IsError,
				"%s must refuse a selector it cannot consume, got: %s", tc.name, toolResultText(result))
			require.Contains(t, toolResultText(result), "unsupported_target")
		})
	}

	// The consuming operations still accept their selectors.
	for i, ok := range []map[string]any{
		{"operation": "usages", "target": map[string]any{"symbol": helperID}},
		{"kind": "impact", "target": map[string]any{"symbol": helperID}},
	} {
		tool := "relations"
		if _, isAnalyze := ok["kind"]; isAnalyze {
			tool = "analyze"
		}
		ok["output"] = map[string]any{"format": "json"}
		result := call(320+i, tool, ok)
		require.Falsef(t, result.IsError, "a consumed selector must still work: %s", toolResultText(result))
	}
}

// TestAnalyzeImpactAcceptsSymbolBatchSelector pins the encoding seam: the
// facade lowers target:{symbols} to a JSON array, and a reader that only
// comma-splits turns that into ids matching nothing — an empty ranking with no
// error, which is the same fail-open in a different disguise.
func TestAnalyzeImpactAcceptsSymbolBatchSelector(t *testing.T) {
	srv, _ := setupTestServer(t)
	ctx := WithSessionID(context.Background(), "impact_symbol_batch")
	call := facadeFrameCaller(t, srv, ctx)
	helperID := fixtureSymbolID(t, srv, ctx, "helper")

	result := call(2, "analyze", map[string]any{
		"kind":   "impact",
		"target": map[string]any{"symbols": []string{helperID}},
		"output": map[string]any{"format": "json"},
	})
	require.False(t, result.IsError, toolResultText(result))
	payload := unmarshalResult(t, result)
	symbols, _ := payload["symbols"].([]any)
	require.NotEmpty(t, symbols, "a symbol batch selector must score its symbols, not silently match nothing")
	require.Contains(t, toolResultText(result), helperID)
}

func TestSplitSymbolIDFieldAcceptsEveryEncoding(t *testing.T) {
	want := []string{"a.go::A", "b.go::B"}
	for name, raw := range map[string]any{
		"json array":        `["a.go::A","b.go::B"]`,
		"comma separated":   "a.go::A,b.go::B",
		"spaced comma list": " a.go::A , b.go::B ",
		"string slice":      []string{"a.go::A", "b.go::B"},
		"any slice":         []any{"a.go::A", "b.go::B"},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, want, splitSymbolIDField(raw))
		})
	}
	require.Empty(t, splitSymbolIDField(""))
	require.Empty(t, splitSymbolIDField(nil))
}

// TestRefusedSelectorNamesTheActualMistake keeps the refusal honest about why
// it fired. Most kinds have no field for the selector, so it would be dropped.
// communities and processes do have an `id` — it just names a community or a
// process, so lowering a symbol into it resolves against the wrong entity and
// returns "community not found: <symbol>", a loud failure that blames the id
// channel for a selector-semantics mistake. The two cases must not share one
// wording.
func TestRefusedSelectorNamesTheActualMistake(t *testing.T) {
	srv, _ := setupTestServer(t)
	ctx := WithSessionID(context.Background(), "refusal_wording")
	call := facadeFrameCaller(t, srv, ctx)
	helperID := fixtureSymbolID(t, srv, ctx, "helper")

	dropped := call(400, "analyze", map[string]any{
		"kind": "routes", "target": map[string]any{"symbol": helperID},
		"output": map[string]any{"format": "json"},
	})
	require.True(t, dropped.IsError)
	require.Contains(t, toolResultText(dropped), "would be ignored",
		"a kind with no field for the selector drops it")
	require.NotContains(t, toolResultText(dropped), "entity_mismatch")

	for i, kind := range []string{"communities", "processes"} {
		mismatched := call(401+i, "analyze", map[string]any{
			"kind": kind, "target": map[string]any{"symbol": helperID},
			"output": map[string]any{"format": "json"},
		})
		require.True(t, mismatched.IsError)
		text := toolResultText(mismatched)
		require.Contains(t, text, "entity_mismatch",
			"analyze(kind=%s) does have an id field — it just is not a symbol", kind)
		require.Contains(t, text, "does not name a symbol")
		require.NotContains(t, text, "would be ignored")

		// The id channel itself still works when given the right entity, and
		// still fails loudly when given the wrong one.
		wrongEntity := call(410+i, "analyze", map[string]any{
			"kind": kind, "options": map[string]any{"id": helperID},
			"output": map[string]any{"format": "json"},
		})
		require.True(t, wrongEntity.IsError)
		require.Contains(t, toolResultText(wrongEntity), "not found")
	}
}
