package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
)

func TestScopeStore_PersistsRoundTrip(t *testing.T) {
	path := filepath.Join(tempSidecarDir(t), "scopes.json")
	st := newScopeStore(path)
	require.NoError(t, st.put(SavedScope{Name: "backend", Description: "be", Repos: []string{"api", "core"}}))

	// A fresh store over the same file sees the persisted scope.
	got, ok := newScopeStore(path).get("backend")
	require.True(t, ok, "a saved scope must survive a store reload")
	require.Equal(t, []string{"api", "core"}, got.Repos)

	removed, err := st.remove("backend")
	require.NoError(t, err)
	require.True(t, removed)
	_, stillThere := newScopeStore(path).get("backend")
	require.False(t, stillThere, "a removed scope must not survive a reload")
}

// TestSavedScope_FiltersSearchResults drives save_scope + a scoped
// search_symbols end-to-end and confirms the scope confines results.
func TestSavedScope_FiltersSearchResults(t *testing.T) {
	t.Setenv("GORTEX_SCOPES_PATH", filepath.Join(tempSidecarDir(t), "scopes.json"))
	srv, _, _ := newIsolationServer(t)
	ctx := context.Background() // unbound session — the scope is the only filter

	saveReq := mcplib.CallToolRequest{}
	saveReq.Params.Name = "save_scope"
	saveReq.Params.Arguments = map[string]any{"name": "alpha-only", "repos": "repo-a"}
	saveRes, err := srv.handleSaveScope(ctx, saveReq)
	require.NoError(t, err)
	require.False(t, saveRes.IsError, "save_scope must succeed")

	resp := scopedSearch(t, srv, ctx, "Thing", "alpha-only")
	ids := resultIDs(resp)
	require.NotEmpty(t, ids, "the scoped search should still find repo-a symbols")
	for _, id := range ids {
		require.NotContainsf(t, id, "repo-b", "scoped search leaked a repo-b result: %s", id)
	}
	require.Contains(t, strings.Join(ids, " "), "AlphaThing")
}

func TestSavedScope_UnknownScopeErrors(t *testing.T) {
	t.Setenv("GORTEX_SCOPES_PATH", filepath.Join(tempSidecarDir(t), "scopes.json"))
	srv, _, _ := newIsolationServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{"query": "Thing", "scope": "no-such-scope"}
	res, err := srv.handleSearchSymbols(context.Background(), req)
	require.NoError(t, err)
	require.True(t, res.IsError, "an unknown scope must surface an error")
}

func scopedSearch(t *testing.T, srv *Server, ctx context.Context, query, scope string) map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = map[string]any{"query": query, "scope": scope}
	res, err := srv.handleSearchSymbols(ctx, req)
	require.NoError(t, err)
	require.Falsef(t, res.IsError, "scoped search %q errored", query)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	return resp
}
