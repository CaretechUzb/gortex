package mcp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func decodeRenameErrorResp(t *testing.T, res *mcplib.CallToolResult) map[string]any {
	t.Helper()
	require.True(t, res.IsError, "%v", res.Content)
	require.NotEmpty(t, res.Content)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	return resp
}

func TestRenameSymbol_UnindexedRecovery(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "package main\n\nfunc Late() {}\nfunc Use() { Late() }\n"
	path := filepath.Join(dir, "late.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))

	for _, dryRun := range []bool{false, true} {
		res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
			"id": "late.go::Late", "new_name": "Renamed", "dry_run": dryRun,
		})
		require.True(t, res.IsError)
		resp := decodeRenameErrorResp(t, res)
		require.Equal(t, "symbol_not_indexed", resp["error_code"])
		recovery := resp["data"].(map[string]any)
		require.Equal(t, "refused", recovery["status"])
		require.Equal(t, false, recovery["semantic_rename_complete"])
		require.Equal(t, false, recovery["written"])
		require.Equal(t, dryRun, recovery["dry_run"])
		require.EqualValues(t, 1, recovery["occurrences"])
		require.Contains(t, recovery["warning"], "Only the parsed declaration is anchored")

		fallback := recovery["safe_fallback"].(map[string]any)
		require.Equal(t, "edit", fallback["tool"])
		require.Equal(t, "declaration_only", fallback["scope"])
		request := fallback["request"].(map[string]any)
		require.Equal(t, "file", request["operation"])
		require.Equal(t, "late.go", request["target"].(map[string]any)["file"])
		require.Equal(t, "func Late() {}\n", request["match"])
		require.Equal(t, "func Renamed() {}\n", request["replacement"])
		guard := request["guard"].(map[string]any)
		require.EqualValues(t, 1, guard["expected_occurrences"])
		require.NotEmpty(t, guard["base_sha"])
		require.NotEmpty(t, fallback["guidance"])

		got, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, source, string(got))
	}
}

func TestRenameSymbol_IgnoredRecovery(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "package main\n\nfunc Ignored() {}\n"
	path := filepath.Join(dir, "ignored.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))
	srv.indexer.SetExcludePatterns([]string{"ignored.go"})
	_, err := srv.indexer.Index(dir)
	require.NoError(t, err)

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id": "ignored.go::Ignored", "new_name": "Renamed",
	})
	require.True(t, res.IsError)
	resp := decodeRenameErrorResp(t, res)
	require.Equal(t, "symbol_not_indexed", resp["error_code"])
	require.EqualValues(t, 1, resp["data"].(map[string]any)["occurrences"])
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source, string(got))
}

func TestRenameSymbol_RecoveryExecutesDeclarationOnly(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "package main\n\ntype Server struct{}\nfunc (s *Server) Run() {}\nfunc Use(s *Server) { s.Run() }\n"
	path := filepath.Join(dir, "late.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id": "late.go::Server.Run", "new_name": "Execute",
	})
	resp := decodeRenameErrorResp(t, res)
	recovery := resp["data"].(map[string]any)
	require.Equal(t, "Run", recovery["declaration_name"])
	fallback := recovery["safe_fallback"].(map[string]any)
	request := fallback["request"].(map[string]any)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "edit"
	req.Params.Arguments = request
	editResult, err := srv.handleFacade(context.Background(), "edit", req)
	require.NoError(t, err)
	require.False(t, editResult.IsError, toolResultText(editResult))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "package main\n\ntype Server struct{}\nfunc (s *Server) Execute() {}\nfunc Use(s *Server) { s.Run() }\n", string(got))
}

func TestRenameSymbol_SubIdentifierIsNotADeclaration(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "const foo$bar = 1;\n"
	path := filepath.Join(dir, "late.js")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))

	res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
		"id": "late.js::foo", "new_name": "renamed",
	})
	require.True(t, res.IsError)
	require.Contains(t, toolResultText(res), "symbol not found: late.js::foo")
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source, string(got))
}

func TestRenameSymbol_InvalidTargetStaysNotFound(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "late.go"), []byte("package main\n\nfunc Late() {}\n"), 0o644))
	for _, id := range []string{"missing.go::Missing", "target.go::Missing", "late.go::Typo"} {
		res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
			"id": id, "new_name": "Renamed",
		})
		require.True(t, res.IsError)
		require.Contains(t, toolResultText(res), "symbol not found: "+id)
	}
	target, err := os.ReadFile(filepath.Join(dir, "target.go"))
	require.NoError(t, err)
	require.Equal(t, renameTargetSrc, string(target))
}
