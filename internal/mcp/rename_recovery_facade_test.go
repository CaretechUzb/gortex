package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestRenameFacade_UnindexedRecovery(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "package main\n\nfunc Late() {}\n"
	path := filepath.Join(dir, "late.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))

	req := mcplib.CallToolRequest{}
	req.Params.Name = "refactor"
	req.Params.Arguments = map[string]any{
		"operation": "rename",
		"target":    map[string]any{"symbol": "late.go::Late"},
		"new_name":  "Renamed",
	}
	res, err := srv.handleFacade(context.Background(), "refactor", req)
	require.NoError(t, err)
	require.True(t, res.IsError)
	require.Equal(t, facadeOutcomeToolError, classifyFacadeOutcome(res, nil))
	resp := decodeRenameErrorResp(t, res)
	require.Equal(t, "symbol_not_indexed", resp["error_code"])
	require.Equal(t, "refused", resp["data"].(map[string]any)["status"])
	require.Equal(t, false, resp["data"].(map[string]any)["written"])

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source, string(got))
}
