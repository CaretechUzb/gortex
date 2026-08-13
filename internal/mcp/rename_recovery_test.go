package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRenameSymbol_UnindexedRecovery(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	const source = "package main\n\nfunc Late() {}\n"
	path := filepath.Join(dir, "late.go")
	require.NoError(t, os.WriteFile(path, []byte(source), 0o644))

	for _, dryRun := range []bool{false, true} {
		res := callToolByName(t, srv, context.Background(), "rename_symbol", map[string]any{
			"id": "late.go::Late", "new_name": "Renamed", "dry_run": dryRun,
		})
		resp := decodeRenameResp(t, res)
		require.Equal(t, "refused", resp["status"])
		require.Equal(t, "symbol_not_indexed", resp["error_code"])
		require.Equal(t, false, resp["semantic_rename_complete"])
		require.Equal(t, false, resp["written"])
		require.Equal(t, dryRun, resp["dry_run"])
		require.Contains(t, resp["warning"], "Cross-file references are not proven")

		fallback := resp["safe_fallback"].(map[string]any)
		require.Equal(t, "edit", fallback["tool"])
		require.Equal(t, "file", fallback["operation"])
		require.Equal(t, "late.go", fallback["target"].(map[string]any)["file"])
		require.ElementsMatch(t, []any{
			"explicit file target", "whole-identifier match", "expected_occurrences", "base_sha",
		}, fallback["required_guards"])

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
	resp := decodeRenameResp(t, res)
	require.Equal(t, "symbol_not_indexed", resp["error_code"])
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source, string(got))
}

func TestRenameSymbol_InvalidTargetStaysNotFound(t *testing.T) {
	srv, dir := setupRenameServer(t, renameTargetSrc, renameCallerSrc)
	for _, id := range []string{"missing.go::Missing", "target.go::Missing"} {
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
