package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

type searchSymbolsFallbackResponse struct {
	Results        []map[string]any `json:"results"`
	Total          int              `json:"total"`
	Decomposed     bool             `json:"decomposed"`
	ContentMatches *struct {
		Reason  string `json:"reason"`
		Query   string `json:"query"`
		Note    string `json:"note"`
		Count   int    `json:"count"`
		Matches []struct {
			Path       string `json:"path"`
			Line       int    `json:"line"`
			Text       string `json:"text"`
			SymbolID   string `json:"symbol_id"`
			SymbolName string `json:"symbol_name"`
		} `json:"matches"`
	} `json:"content_matches"`
}

// setupContentFallbackServer indexes a repo whose only occurrences of
// GORTEX_GENERATED_HANDLER and gortexnoseparatorhandler sit inside a function
// body, so both identifiers exist verbatim in file content but neither becomes
// a symbol node. The first carries separators and so reaches the caller through
// the leaf-decomposition rescue; the second has none and returns a literally
// empty page. The comment on line 3 is the prose control: it too occurs
// verbatim in content, and its words name no symbol in the fixture.
func setupContentFallbackServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "dispatch.go"), []byte(
		"package app\n\n"+
			"// wiring table for generated handlers\n"+
			"func Register() {\n"+
			"\troute(\"GORTEX_GENERATED_HANDLER\")\n"+
			"\troute(\"gortexnoseparatorhandler\")\n"+
			"}\n\n"+
			"func route(name string) {}\n"),
		0o644))

	g := graph.New()
	reg := testRegistry()
	cfg := config.Default()
	idx := indexer.New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	eng := query.NewEngine(g)
	return NewServer(eng, g, idx, nil, zap.NewNop(), nil)
}

func decodeSearchSymbolsFallback(t *testing.T, res *mcplib.CallToolResult) searchSymbolsFallbackResponse {
	t.Helper()
	require.False(t, res.IsError)
	var out searchSymbolsFallbackResponse
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &out))
	return out
}

func requireNoSymbolRowNamed(t *testing.T, out searchSymbolsFallbackResponse, name string) {
	t.Helper()
	for _, row := range out.Results {
		require.NotEqual(t, name, row["name"], "fixture invariant: the identifier must have no symbol node")
	}
}

// TestSearchSymbols_ContentFallbackOnDecomposedMiss pins the recovery path for
// a separator-carrying identifier: the exact lookup matches no symbol, the
// decomposition rescue answers with leaf-token neighbours, and the identifier
// itself comes back as clearly-labelled content evidence.
func TestSearchSymbols_ContentFallbackOnDecomposedMiss(t *testing.T) {
	srv := setupContentFallbackServer(t)

	out := decodeSearchSymbolsFallback(t, callTool(t, srv, "search_symbols",
		map[string]any{"query": "GORTEX_GENERATED_HANDLER"}))
	require.True(t, out.Decomposed, "fixture invariant: the exact identifier resolves to no symbol node")
	requireNoSymbolRowNamed(t, out, "GORTEX_GENERATED_HANDLER")

	require.NotNil(t, out.ContentMatches, "an exact identifier miss with content presence must fall back to text search")
	require.Equal(t, "symbol_lookup_empty", out.ContentMatches.Reason)
	require.Equal(t, "GORTEX_GENERATED_HANDLER", out.ContentMatches.Query)
	require.NotEmpty(t, out.ContentMatches.Note, "the section must say these are content matches, not symbol nodes")
	require.GreaterOrEqual(t, out.ContentMatches.Count, 1)
	require.LessOrEqual(t, out.ContentMatches.Count, searchSymbolsContentFallbackMaxMatches)
	require.Len(t, out.ContentMatches.Matches, out.ContentMatches.Count)

	hit := out.ContentMatches.Matches[0]
	require.Equal(t, "dispatch.go", hit.Path)
	require.Equal(t, 5, hit.Line)
	require.Contains(t, hit.Text, "GORTEX_GENERATED_HANDLER")
	require.Equal(t, "Register", hit.SymbolName,
		"a content match must be mapped to its smallest enclosing declaration")
	require.NotEmpty(t, hit.SymbolID)
}

// TestSearchSymbols_ContentFallbackOnEmptyPage covers the other half of the
// same miss: an identifier with no separator to decompose returns a literally
// empty symbol page, and still recovers its content evidence.
func TestSearchSymbols_ContentFallbackOnEmptyPage(t *testing.T) {
	srv := setupContentFallbackServer(t)

	out := decodeSearchSymbolsFallback(t, callTool(t, srv, "search_symbols",
		map[string]any{"query": "gortexnoseparatorhandler"}))
	require.Equal(t, 0, out.Total, "fixture invariant: the identifier must have no symbol node")
	require.Empty(t, out.Results, "the fallback must not leak into the symbol result rows")

	require.NotNil(t, out.ContentMatches)
	require.Equal(t, "symbol_lookup_empty", out.ContentMatches.Reason)
	require.Equal(t, "gortexnoseparatorhandler", out.ContentMatches.Query)
	require.Equal(t, 1, out.ContentMatches.Count)

	hit := out.ContentMatches.Matches[0]
	require.Equal(t, "dispatch.go", hit.Path)
	require.Equal(t, 6, hit.Line)
	require.Equal(t, "Register", hit.SymbolName)
}

// TestSearchSymbols_NoContentFallbackWhenSymbolsFound keeps the fallback off
// the common path: a query that resolves to graph symbols pays no trigram
// search and the response shape is unchanged.
func TestSearchSymbols_NoContentFallbackWhenSymbolsFound(t *testing.T) {
	srv := setupContentFallbackServer(t)

	out := decodeSearchSymbolsFallback(t, callTool(t, srv, "search_symbols",
		map[string]any{"query": "Register"}))
	require.Greater(t, out.Total, 0)
	require.False(t, out.Decomposed)
	require.Nil(t, out.ContentMatches, "non-empty symbol results must not trigger the content fallback")
}

// TestSearchSymbols_NoContentFallbackForNonIdentifierQuery keeps the fallback
// anchored to exact-identifier lookups. The prose query below occurs verbatim
// in the fixture's content, so only the shape gate can suppress the section.
func TestSearchSymbols_NoContentFallbackForNonIdentifierQuery(t *testing.T) {
	srv := setupContentFallbackServer(t)

	out := decodeSearchSymbolsFallback(t, callTool(t, srv, "search_symbols",
		map[string]any{"query": "wiring table for generated handlers"}))
	require.Nil(t, out.ContentMatches, "a multi-word prose query is not identifier-shaped")
}

// TestSearchSymbols_NoContentFallbackUnderKindFilter keeps the fallback out of
// a narrowed lookup: a raw content line carries no node kind, so it can never
// satisfy the constraint the caller asked for.
func TestSearchSymbols_NoContentFallbackUnderKindFilter(t *testing.T) {
	srv := setupContentFallbackServer(t)

	out := decodeSearchSymbolsFallback(t, callTool(t, srv, "search_symbols",
		map[string]any{"query": "GORTEX_GENERATED_HANDLER", "kind": "function"}))
	requireNoSymbolRowNamed(t, out, "GORTEX_GENERATED_HANDLER")
	require.Nil(t, out.ContentMatches, "a kind-filtered lookup must not receive kindless content rows")
}

func TestSearchSymbolsContentFallbackTerm(t *testing.T) {
	for _, tc := range []struct {
		query string
		term  string
		ok    bool
	}{
		{"GORTEX_GENERATED_HANDLER", "GORTEX_GENERATED_HANDLER", true},
		{"  parseToken  ", "parseToken", true},
		{"`wrapped_ident`", "wrapped_ident", true},
		{"handler$impl", "handler$impl", true},
		{"", "", false},
		{"ab", "", false},
		{"decode bson body", "", false},
		{"internal/mcp/tools_core.go", "", false},
		{"Parse(ctx)", "", false},
		{"UserService.FindUser", "", false},
		{"____", "", false},
		{"9lives", "", false},
	} {
		term, ok := searchSymbolsContentFallbackTerm(tc.query)
		require.Equal(t, tc.ok, ok, "query %q", tc.query)
		require.Equal(t, tc.term, term, "query %q", tc.query)
	}
}
