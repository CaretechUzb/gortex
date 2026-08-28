package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
)

// executorTestServer builds a real Server (core/defer preset) with a
// one-file indexed repo, returning the server and the local executor.
func executorTestServer(t *testing.T) (*gortexmcp.Server, daemon.LocalExecutor) {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte(`package main

func main() {}
`), 0o644))

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := indexer.New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	eng := query.NewEngine(g)
	srv := gortexmcp.NewServer(eng, g, idx, nil, zap.NewNop(), nil)
	return srv, newLocalToolExecutor(srv, zap.NewNop())
}

// TestLocalExecutor_MalformedJSONRejectedBeforePromotion pins reviewer
// concern #3: malformed federation JSON must 400 without promoting the
// tool or running its handler.
func TestLocalExecutor_MalformedJSONRejectedBeforePromotion(t *testing.T) {
	srv, exec := executorTestServer(t)
	handlerRan := false
	srv.MCPServer().AddTool(
		mcp.NewTool("probe_tool", mcp.WithDescription("test")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			handlerRan = true
			return mcp.NewToolResultText("ran"), nil
		},
	)

	out, status, err := exec(context.Background(), "probe_tool", []byte("{bad json"))
	require.NoError(t, err)
	assert.Equal(t, 400, status)
	assert.Contains(t, string(out), "invalid_json")
	assert.False(t, handlerRan, "malformed input must not run the handler")
}

// TestLocalExecutor_MalformedFlatArgsRejected covers the second parse
// branch: a body that is neither a nested {"arguments":...} object nor
// a flat JSON object is rejected too.
func TestLocalExecutor_MalformedFlatArgsRejected(t *testing.T) {
	srv, exec := executorTestServer(t)
	handlerRan := false
	srv.MCPServer().AddTool(
		mcp.NewTool("probe_tool", mcp.WithDescription("test")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			handlerRan = true
			return mcp.NewToolResultText("ran"), nil
		},
	)

	out, status, err := exec(context.Background(), "probe_tool", []byte(`[1,2,3]`))
	require.NoError(t, err)
	assert.Equal(t, 400, status)
	assert.Contains(t, string(out), "invalid_json")
	assert.False(t, handlerRan, "malformed input must not run the handler")
}

// TestLocalExecutor_ValidNestedArgsDispatches covers the happy path: a
// well-formed {"arguments": {...}} body reaches the tool handler.
func TestLocalExecutor_ValidNestedArgsDispatches(t *testing.T) {
	srv, exec := executorTestServer(t)
	srv.MCPServer().AddTool(
		mcp.NewTool("echo_args", mcp.WithDescription("test")),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			msg, _ := req.GetArguments()["message"].(string)
			return mcp.NewToolResultText("got:" + msg), nil
		},
	)

	out, status, err := exec(context.Background(), "echo_args", []byte(`{"arguments":{"message":"hi"}}`))
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	assert.Contains(t, string(out), "got:hi")
}

// TestLocalExecutor_ValidFlatArgsDispatches covers the flat-args body
// shape the executor accepts alongside the nested envelope.
func TestLocalExecutor_ValidFlatArgsDispatches(t *testing.T) {
	srv, exec := executorTestServer(t)
	srv.MCPServer().AddTool(
		mcp.NewTool("echo_args", mcp.WithDescription("test")),
		func(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			msg, _ := req.GetArguments()["message"].(string)
			return mcp.NewToolResultText("flat:" + msg), nil
		},
	)

	out, status, err := exec(context.Background(), "echo_args", []byte(`{"message":"hi"}`))
	require.NoError(t, err)
	assert.Equal(t, 200, status)
	assert.Contains(t, string(out), "flat:hi")
}

// TestLocalExecutor_UnknownTool404 keeps the not-found contract for a
// name that is neither live nor deferred.
func TestLocalExecutor_UnknownTool404(t *testing.T) {
	_, exec := executorTestServer(t)
	out, status, err := exec(context.Background(), "no_such_tool", []byte(`{}`))
	require.NoError(t, err)
	assert.Equal(t, 404, status)
	assert.Contains(t, string(out), "tool_not_found")
}
