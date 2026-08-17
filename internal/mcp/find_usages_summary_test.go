package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// usagesSummaryServer builds a server whose graph references `Foo` from
// four call sites across three files — two in pkg/a.go, one in pkg/b.go,
// and one in a *_test.go caller flagged is_test — plus an unreferenced
// symbol. So a complete find_usages over Foo must report n_refs=4,
// n_files=3, n_test_refs=1, and the unreferenced symbol must carry no
// summary at all.
func usagesSummaryServer(t *testing.T) (srv *Server, fooID, unusedID string) {
	t.Helper()
	g := graph.New()
	foo := &graph.Node{ID: "pkg/foo.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "pkg/foo.go", StartLine: 1}
	unused := &graph.Node{ID: "pkg/foo.go::Unused", Kind: graph.KindFunction, Name: "Unused", FilePath: "pkg/foo.go", StartLine: 20}
	use1 := &graph.Node{ID: "pkg/a.go::Use1", Kind: graph.KindFunction, Name: "Use1", FilePath: "pkg/a.go", StartLine: 3}
	use2 := &graph.Node{ID: "pkg/a.go::Use2", Kind: graph.KindFunction, Name: "Use2", FilePath: "pkg/a.go", StartLine: 8}
	use3 := &graph.Node{ID: "pkg/b.go::Use3", Kind: graph.KindFunction, Name: "Use3", FilePath: "pkg/b.go", StartLine: 5}
	testUse := &graph.Node{
		ID: "pkg/foo_test.go::TestUse", Kind: graph.KindFunction, Name: "TestUse",
		FilePath: "pkg/foo_test.go", StartLine: 12, Meta: map[string]any{"is_test": true},
	}
	for _, n := range []*graph.Node{foo, unused, use1, use2, use3, testUse} {
		g.AddNode(n)
	}
	g.AddEdge(&graph.Edge{From: use1.ID, To: foo.ID, Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 3})
	g.AddEdge(&graph.Edge{From: use2.ID, To: foo.ID, Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 8})
	g.AddEdge(&graph.Edge{From: use3.ID, To: foo.ID, Kind: graph.EdgeCalls, FilePath: "pkg/b.go", Line: 5})
	g.AddEdge(&graph.Edge{From: testUse.ID, To: foo.ID, Kind: graph.EdgeCalls, FilePath: "pkg/foo_test.go", Line: 12})

	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil), foo.ID, unused.ID
}

func findUsagesText(t *testing.T, srv *Server, args map[string]any) string {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = args
	res, err := srv.handleFindUsages(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	return res.Content[0].(mcplib.TextContent).Text
}

// TestFindUsages_SummaryJSON pins the completeness rollup on the plain
// JSON path: the split (refs / files / test refs) an agent needs to know
// the usage list already covers tests.
func TestFindUsages_SummaryJSON(t *testing.T) {
	srv, fooID, unusedID := usagesSummaryServer(t)

	var resp struct {
		UsageSummary *query.UsageSummary `json:"usage_summary"`
	}
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": fooID})), &resp))
	require.NotNil(t, resp.UsageSummary, "usage_summary must be present for a referenced symbol")
	require.Equal(t, 4, resp.UsageSummary.NRefs, "n_refs")
	require.Equal(t, 3, resp.UsageSummary.NFiles, "n_files")
	require.Equal(t, 1, resp.UsageSummary.NTestRefs, "n_test_refs")

	// A symbol with no usages carries the zero-edge caveat, not an
	// all-zero summary — the summary must be omitted entirely.
	var empty map[string]any
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": unusedID})), &empty))
	_, has := empty["usage_summary"]
	require.False(t, has, "usage_summary must be omitted for a symbol with no usages")
}

// TestFindUsages_SummaryGCX pins the rollup on the GCX wire path: the
// three counts ride in the response meta.
func TestFindUsages_SummaryGCX(t *testing.T) {
	srv, fooID, _ := usagesSummaryServer(t)
	out := findUsagesText(t, srv, map[string]any{"id": fooID, "format": "gcx"})
	require.Contains(t, out, "n_refs=4")
	require.Contains(t, out, "n_files=3")
	require.Contains(t, out, "n_test_refs=1")
}

// TestFindUsages_SummaryTOON pins the rollup on the TOON wire path.
func TestFindUsages_SummaryTOON(t *testing.T) {
	srv, fooID, _ := usagesSummaryServer(t)
	out := findUsagesText(t, srv, map[string]any{"id": fooID, "format": "toon"})
	require.Contains(t, out, "usage_summary")
	require.Contains(t, out, "n_refs")
	require.Contains(t, out, "n_test_refs")
	// The tabular edge section must still be present alongside the rollup.
	require.True(t, strings.Contains(out, "edges") || strings.Contains(out, "nodes"),
		"TOON must retain the subgraph body, not just the summary")
}
