package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #597: tool schemas promise additionalProperties:false but dispatch read
// only the keys it knew — an unknown option (line_range on read_file)
// vanished silently and the caller paid the full un-windowed response with
// no signal to self-correct. The guard rejects at dispatch, BEFORE the
// handler runs, naming the unknown keys and the valid ones.

func guardFixture(t *testing.T) (mcp.Tool, *int) {
	t.Helper()
	tool := mcp.NewTool("probe_tool",
		mcp.WithString("offset", mcp.Description("window start")),
		mcp.WithNumber("limit", mcp.Description("window size")),
	)
	// NewTool leaves additionalProperties unset; prepareTool closes it.
	// The guard-unit fixture models the post-registration state.
	tool.InputSchema.AdditionalProperties = false
	calls := 0
	return tool, &calls
}

func guardHandler(calls *int) func(context.Context, mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	return func(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		*calls++
		return mcp.NewToolResultText("ok"), nil
	}
}

func callGuarded(t *testing.T, tool mcp.Tool, calls *int, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	wrapped := wrapToolArgGuard(tool, guardHandler(calls))
	req := mcp.CallToolRequest{}
	req.Params.Name = tool.Name
	req.Params.Arguments = args
	res, err := wrapped(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	return res
}

func guardResultText(res *mcp.CallToolResult) string {
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			sb.WriteString(tc.Text)
		}
	}
	return sb.String()
}

func TestToolArgGuard_UnknownKeyRejectedBeforeHandler(t *testing.T) {
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, map[string]any{
		"offset":     "10",
		"line_range": []any{120, 160},
	})
	assert.True(t, res.IsError, "an unknown option must reject the call, not vanish")
	assert.Zero(t, *calls, "the handler must never run — that is the whole point: the expensive wrong answer is never produced")
	text := guardResultText(res)
	assert.Contains(t, text, "line_range", "the error names the unknown key")
	assert.Contains(t, text, "offset", "the error lists the valid keys")
	assert.Contains(t, text, "limit", "the error lists the valid keys")
}

func TestToolArgGuard_ValidKeysPassThrough(t *testing.T) {
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, map[string]any{"offset": "10", "limit": 40})
	assert.False(t, res.IsError)
	assert.Equal(t, 1, *calls)
}

func TestToolArgGuard_NilArgumentsPassThrough(t *testing.T) {
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, nil)
	assert.False(t, res.IsError)
	assert.Equal(t, 1, *calls)
}

// A tool that explicitly opens its top-level schema opts out of the guard —
// the contract is whatever the schema says, in both directions.
func TestToolArgGuard_ExplicitOpenSchemaUnenforced(t *testing.T) {
	tool, calls := guardFixture(t)
	tool.InputSchema.AdditionalProperties = true
	res := callGuarded(t, tool, calls, map[string]any{"anything": 1})
	assert.False(t, res.IsError)
	assert.Equal(t, 1, *calls)
}

// Raw-schema tools (the facade envelopes) are enforced exactly as authored:
// additionalProperties:false rejects, true stays open.
func TestToolArgGuard_RawSchemaHonorsAuthoredContract(t *testing.T) {
	closed := mcp.NewToolWithRawSchema("closed_tool", "d", json.RawMessage(
		`{"type":"object","properties":{"operation":{"type":"string"}},"additionalProperties":false}`))
	open := mcp.NewToolWithRawSchema("open_tool", "d", json.RawMessage(
		`{"type":"object","properties":{"operation":{"type":"string"}},"additionalProperties":true}`))

	calls := 0
	res := callGuarded(t, closed, &calls, map[string]any{"operaton": "file"})
	assert.True(t, res.IsError, "typo'd key on a closed raw schema must reject")
	assert.Zero(t, calls)

	res = callGuarded(t, open, &calls, map[string]any{"extra": true})
	assert.False(t, res.IsError, "an open raw schema accepts extras by contract")
	assert.Equal(t, 1, calls)
}

func TestToolArgGuard_WarnModeCallsHandlerWithRider(t *testing.T) {
	t.Setenv(toolArgGuardEnv, "warn")
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, map[string]any{"line_range": []any{1, 2}})
	assert.False(t, res.IsError)
	assert.Equal(t, 1, *calls, "warn mode still runs the handler")
	assert.Contains(t, guardResultText(res), "line_range", "the rider names what was ignored")
}

func TestToolArgGuard_OffModeDisables(t *testing.T) {
	t.Setenv(toolArgGuardEnv, "off")
	tool, calls := guardFixture(t)
	res := callGuarded(t, tool, calls, map[string]any{"line_range": []any{1, 2}})
	assert.False(t, res.IsError)
	assert.Equal(t, 1, *calls)
	assert.NotContains(t, guardResultText(res), "line_range")
}

// prepareTool closes any structured schema that never took a position, so
// the published contract says what dispatch now enforces. Raw schemas are
// left exactly as authored.
func TestPrepareToolStampsClosedSchema(t *testing.T) {
	srv, _ := setupTestServer(t)
	tool := mcp.NewTool("stamp_probe_tool", mcp.WithString("offset", mcp.Description("window start")))
	require.Nil(t, tool.InputSchema.AdditionalProperties)
	srv.prepareTool(&tool, guardHandler(new(int)))

	out, err := json.Marshal(tool)
	require.NoError(t, err)
	var m struct {
		InputSchema struct {
			AdditionalProperties any `json:"additionalProperties"`
		} `json:"inputSchema"`
	}
	require.NoError(t, json.Unmarshal(out, &m))
	assert.Equal(t, false, m.InputSchema.AdditionalProperties,
		"an unset structured schema is published closed, matching enforcement")
}

// The issue's measured shape, end to end through real MCP frames on the
// full tool surface: the exact read_file(line_range: ...) call that used
// to return the whole file silently now refuses before the read, and the
// corrected call still works against the real handler.
func TestReadFileRejectsUnknownWindowOptionE2E(t *testing.T) {
	t.Setenv("GORTEX_TOOLS", "full") // legacy names are session-gated off the default surface
	srv, _ := setupTestServer(t)
	ctx := WithSessionID(context.Background(), "arg_guard_e2e")
	initFrame := []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"integration-harness","version":"1.0"}}}`)
	require.NotNil(t, srv.MCPServer().HandleMessage(ctx, initFrame))

	call := func(id int, name string, arguments map[string]any) *mcp.CallToolResult {
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
			Error  any                 `json:"error"`
			Result *mcp.CallToolResult `json:"result"`
		}
		require.NoError(t, json.Unmarshal(raw, &envelope))
		require.Nil(t, envelope.Error, "protocol error: %v", envelope.Error)
		require.NotNil(t, envelope.Result)
		return envelope.Result
	}

	res := call(2, "read_file", map[string]any{"path": "main.go", "line_range": []any{120, 160}})
	require.True(t, res.IsError, "line_range must refuse, not return the full file: %s", guardResultText(res))
	require.Contains(t, guardResultText(res), "line_range")
	require.NotContains(t, guardResultText(res), "func helper", "no file content may ride an arg-guard refusal")

	res = call(3, "read_file", map[string]any{"path": "main.go"})
	require.False(t, res.IsError, guardResultText(res))
}
