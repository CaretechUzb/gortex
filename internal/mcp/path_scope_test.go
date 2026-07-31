package mcp

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

func TestApplyPathFilter_AnchoredPrefix(t *testing.T) {
	mk := func(path string) *graph.Node {
		return &graph.Node{ID: path + "::S", Kind: graph.KindFunction, Name: "S", FilePath: path}
	}
	nodes := []*graph.Node{
		mk("services/billing/invoice.go"),   // under services/billing
		mk("services/billing/v2/refund.go"), // under services/billing (deeper)
		mk("services/billingX/other.go"),    // NOT under -- the prefix must align on a boundary
		mk("other/services/billing/x.go"),   // NOT under -- prefix is anchored at the start
		mk("libs/auth/token.go"),            // under libs/auth
	}

	got := applyPathFilter(nodes, []string{"services/billing"})
	gotPaths := map[string]bool{}
	for _, n := range got {
		gotPaths[n.FilePath] = true
	}
	require.True(t, gotPaths["services/billing/invoice.go"])
	require.True(t, gotPaths["services/billing/v2/refund.go"])
	require.False(t, gotPaths["services/billingX/other.go"], "billingX must not match the billing prefix")
	require.False(t, gotPaths["other/services/billing/x.go"], "the prefix is anchored at the path start")
	require.False(t, gotPaths["libs/auth/token.go"])

	// Multi-path: union of two prefixes.
	multi := applyPathFilter(nodes, []string{"services/billing", "libs/auth"})
	require.Len(t, multi, 3)

	// Slash normalisation: leading "./", trailing slash, back-slashes.
	norm := applyPathFilter(nodes, []string{"./services/billing/"})
	require.Len(t, norm, 2)

	// Empty filter is a no-op.
	require.Len(t, applyPathFilter(nodes, nil), len(nodes))
}

func TestApplyPathFilter_StripsRepoPrefix(t *testing.T) {
	// In multi-repo mode FilePath is repo-prefixed; the filter is
	// expressed relative to the repo root and must still match.
	n := &graph.Node{
		ID: "x", Kind: graph.KindFunction, Name: "S",
		FilePath:   "billing-repo/services/billing/invoice.go",
		RepoPrefix: "billing-repo",
	}
	got := applyPathFilter([]*graph.Node{n}, []string{"services/billing"})
	require.Len(t, got, 1, "repo prefix should be stripped before the anchored match")
}

func TestSavedScope_PathsJSONRoundTrip(t *testing.T) {
	path := filepath.Join(tempSidecarDir(t), "scopes.json")
	st := newScopeStore(path)
	require.NoError(t, st.put(SavedScope{
		Name:  "billing",
		Repos: []string{"monorepo"},
		Paths: []string{"services/billing", "libs/money"},
	}))
	got, ok := newScopeStore(path).get("billing")
	require.True(t, ok)
	require.Equal(t, []string{"services/billing", "libs/money"}, got.Paths)

	// A scope with no Paths round-trips with the field omitted (the
	// back-compat shape) -- decoding an old scopes.json still works.
	require.NoError(t, st.put(SavedScope{Name: "repo-only", Repos: []string{"r"}}))
	plain, ok := newScopeStore(path).get("repo-only")
	require.True(t, ok)
	require.Nil(t, plain.Paths)
}

func TestResolvePathFilter_Sources(t *testing.T) {
	t.Setenv("GORTEX_SCOPES_PATH", filepath.Join(tempSidecarDir(t), "scopes.json"))
	g := graph.New()
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewBM25())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	require.NoError(t, srv.scopeStoreOrInit().put(SavedScope{
		Name: "billing", Repos: []string{"r"}, Paths: []string{"services/billing"},
	}))

	// From the explicit path argument.
	req := mcplib.CallToolRequest{}
	req.Params.Arguments = map[string]any{"path": "libs/auth, libs/money"}
	got := srv.resolvePathFilter(req, fieldQuery{})
	require.ElementsMatch(t, []string{"libs/auth", "libs/money"}, got)

	// From an inline path: clause.
	got = srv.resolvePathFilter(mcplib.CallToolRequest{}, fieldQuery{Path: "internal/auth"})
	require.Equal(t, []string{"internal/auth"}, got)

	// From a path-bearing saved scope.
	req2 := mcplib.CallToolRequest{}
	req2.Params.Arguments = map[string]any{"scope": "billing"}
	got = srv.resolvePathFilter(req2, fieldQuery{})
	require.Equal(t, []string{"services/billing"}, got)

	// All three union together.
	req3 := mcplib.CallToolRequest{}
	req3.Params.Arguments = map[string]any{"path": "a/b", "scope": "billing"}
	got = srv.resolvePathFilter(req3, fieldQuery{Path: "c/d"})
	require.ElementsMatch(t, []string{"a/b", "c/d", "services/billing"}, got)
}

// pathScopeServer builds a single-repo server with symbols spread
// across distinct sub-paths so a path filter is observable.
func pathScopeServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	bm := search.NewBM25()
	files := map[string]string{
		"services/billing/Invoice.go": "BillingInvoice",
		"services/auth/Login.go":      "AuthLogin",
		"libs/money/Amount.go":        "MoneyAmount",
	}
	for path, name := range files {
		id := path + "::" + name
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: path, StartLine: 1, EndLine: 5, Language: "go",
		})
		bm.Add(id, name, path, "")
	}
	eng := query.NewEngine(g)
	eng.SetSearch(bm)
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil)
}

// TestSearchSymbols_PathScoping confirms the path argument confines
// search_symbols results to the named sub-path.
func TestSearchSymbols_PathScoping(t *testing.T) {
	srv := pathScopeServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	// Query a token every symbol's path shares -- only the path
	// filter should confine the result.
	req.Params.Arguments = map[string]any{"query": "BillingInvoice OR AuthLogin OR MoneyAmount", "path": "services/billing"}
	res, err := srv.handleSearchSymbols(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	for _, r := range resp["results"].([]any) {
		fp := r.(map[string]any)["file_path"].(string)
		require.Contains(t, fp, "services/billing", "path scoping leaked a result outside services/billing: %s", fp)
	}
	require.NotEmpty(t, resp["results"], "the in-scope billing symbol should still be found")
}

// TestSmartContext_PathScoping confirms smart_context honours the
// path argument.
func TestSmartContext_PathScoping(t *testing.T) {
	srv := pathScopeServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "smart_context"
	req.Params.Arguments = map[string]any{"task": "work on billing invoice login amount", "path": "services/billing"}
	res, err := srv.handleSmartContext(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	if syms, ok := resp["relevant_symbols"].([]any); ok {
		for _, s := range syms {
			if fp, ok := s.(map[string]any)["file_path"].(string); ok && fp != "" {
				require.Contains(t, fp, "services/billing",
					"smart_context path scoping leaked %s", fp)
			}
		}
	}
}
