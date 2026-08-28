package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAnalyzeAliasedKindFromLegacySession pins the dashboard fix: a plain
// analyze(kind=processes) call from a NON-facade session (the HTTP
// dashboard path, which has no session policy) must route through the
// facade to the captured get_processes legacy handler instead of falling
// into the analyze dispatcher's "unknown analyze kind" error. This is the
// reviewer-required replacement for generic registry promotion.
func TestAnalyzeAliasedKindFromLegacySession(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := context.Background()

	// The legacy tool is deferred under core/defer — the facade must
	// reach it without promoting it into the live registry.
	require.True(t, srv.lazy.IsDeferred("get_processes"))

	call := facadeFrameCaller(t, srv, ctx)
	res := call(400, "analyze", map[string]any{"kind": "processes"})
	require.False(t, res.IsError, "analyze kind=processes must not error: %s", toolResultText(res))
	require.Contains(t, toolResultText(res), "processes",
		"the facade must reach the get_processes handler's JSON payload")

	// The legacy tool must NOT have been promoted into the live registry.
	require.True(t, srv.lazy.IsDeferred("get_processes"),
		"facade dispatch must not promote the legacy tool")
	require.Nil(t, srv.MCPServer().GetTool("get_processes"))
}

// TestAnalyzeAliasedKindWithIDReachesProcessDetail covers the web app's
// processDetail path: analyze(kind=processes, id=...) must forward the id
// to the legacy handler.
func TestAnalyzeAliasedKindWithIDReachesProcessDetail(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := context.Background()

	call := facadeFrameCaller(t, srv, ctx)
	res := call(401, "analyze", map[string]any{"kind": "processes", "id": "proc_1"})
	require.False(t, res.IsError, "analyze kind=processes with id must not error: %s", toolResultText(res))
	require.Contains(t, toolResultText(res), "processes")
}

// TestAnalyzeNativeKindStillUsesDispatcher keeps the non-aliased kinds on
// the dispatcher path: hotspots is a native analyze kind and must NOT be
// rerouted through the facade (its behavior is unchanged).
func TestAnalyzeNativeKindStillUsesDispatcher(t *testing.T) {
	srv := setupPresetServer(t, ToolPolicyConfig{Preset: "core", Mode: "defer"})
	ctx := context.Background()

	call := facadeFrameCaller(t, srv, ctx)
	res := call(402, "analyze", map[string]any{"kind": "hotspots"})
	// Hotspots is a native kind — the dispatcher answers it. On the tiny
	// fixture it may report "codebase too small", which is a dispatcher
	// result, never an unknown-kind error.
	require.NotContains(t, toolResultText(res), "unknown analyze kind")
}