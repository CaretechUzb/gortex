package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// callToolFrame dispatches one tools/call frame through the real MCP server
// entry point the daemon dispatcher uses and returns the marshalled reply.
func callToolFrame(t *testing.T, srv *Server, ctx context.Context, tool string, args map[string]any) string {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	frame, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	require.NoError(t, err)
	reply := srv.MCPServer().HandleMessage(ctx, json.RawMessage(frame))
	require.NotNil(t, reply)
	out, err := json.Marshal(reply)
	require.NoError(t, err)
	return string(out)
}

// TestPromotedToolCallableUnderCorePreset covers the by-name reachability
// contract: a tool held out of a session's tools/list (the `core` defer preset)
// stays callable by name after the dispatcher promotes it.
//
// Regression: mcp-go v0.55.1 started re-running registered tool filters at call
// time (server.passesToolFilters). toolSurfaceFilter shapes tools/list
// visibility, so every non-preset tool — 141 of 178 under `core` — began
// answering "tool '<name>' not found: tool not found" for CLI (`gortex call`)
// and for any agent session that forwarded a preset, including calls to tools
// tools_search had just promoted.
func TestPromotedToolCallableUnderCorePreset(t *testing.T) {
	srv, _ := setupTestServer(t)

	const sessionID = "cli_core_defer"
	const toolName = "export_context"
	srv.NoteSessionToolPolicy(sessionID, "core", "defer")
	ctx := WithSessionID(context.Background(), sessionID)

	require.Equal(t, "deferred", srv.sessionToolStatus(ctx, toolName),
		"precondition: export_context is deferred under core/defer")
	require.True(t, srv.IsToolEnabledForSession(ctx, toolName),
		"precondition: the dispatcher's promote gate permits this call")

	// What the daemon dispatcher does before HandleMessage.
	srv.EnsureToolPromotedForSession(ctx, toolName)
	ctx = WithAuthorizedToolCall(ctx, toolName)

	out := callToolFrame(t, srv, ctx, toolName, map[string]any{"task": "cli flags"})
	require.NotContains(t, out, "tool not found",
		"a promoted tool must be callable by name under core/defer:\n%s", clip(out))
}

// TestAuthorizedCallMarkerKeepsHideModeEnforced proves the carve-out cannot be
// used to widen a session's surface: a hide-mode preset still refuses a tool
// outside its allow-set even when the marker is present, and the refusal is the
// structured blocked-by-mode error rather than a misleading "not found".
func TestAuthorizedCallMarkerKeepsHideModeEnforced(t *testing.T) {
	srv, _ := setupTestServer(t)

	const sessionID = "hide_mode_client"
	const toolName = "export_context"
	srv.NoteSessionToolPolicy(sessionID, "nav", "hide")
	ctx := WithSessionID(context.Background(), sessionID)

	require.False(t, srv.IsToolEnabledForSession(ctx, toolName),
		"precondition: a hide-mode session must not be allowed to call a non-preset tool")

	// The dispatcher would never mark this call; set it anyway so the test
	// covers the gate rather than the dispatcher's restraint.
	out := callToolFrame(t, srv, WithAuthorizedToolCall(ctx, toolName), toolName, nil)
	require.Contains(t, out, string(ErrCodeToolBlockedByMode),
		"hide mode must still refuse the call:\n%s", clip(out))
}

// TestAuthorizedCallMarkerDoesNotWidenToolsList pins the marker to the
// call-time single-tool probe: a tools/list rendered on the same context still
// shows only the session's preset surface.
func TestAuthorizedCallMarkerDoesNotWidenToolsList(t *testing.T) {
	srv, _ := setupTestServer(t)

	const sessionID = "list_under_marker"
	const toolName = "export_context"
	srv.NoteSessionToolPolicy(sessionID, "core", "defer")
	ctx := WithSessionID(context.Background(), sessionID)
	srv.EnsureToolPromotedForSession(ctx, toolName)
	ctx = WithAuthorizedToolCall(ctx, toolName)

	reply := srv.MCPServer().HandleMessage(ctx,
		json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	out, err := json.Marshal(reply)
	require.NoError(t, err)

	var parsed struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	require.NoError(t, json.Unmarshal(out, &parsed))
	for _, tool := range parsed.Result.Tools {
		require.NotEqual(t, toolName, tool.Name,
			"the authorized-call marker must not leak a non-preset tool into tools/list")
	}
	require.NotEmpty(t, parsed.Result.Tools, "the preset surface should still render")
}

func clip(s string) string {
	if len(s) <= 400 {
		return s
	}
	return strings.TrimSpace(s[:400]) + "…"
}
