package mcp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// usagesLimitServer builds a server whose graph references `Hot` from
// callerCount distinct call sites, one caller per file, so the limit
// tests can count returned usage rows exactly.
func usagesLimitServer(t *testing.T, callerCount int) (srv *Server, hotID string) {
	t.Helper()
	g := graph.New()
	hot := &graph.Node{ID: "pkg/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "pkg/hot.go", StartLine: 1}
	g.AddNode(hot)
	for i := 0; i < callerCount; i++ {
		file := fmt.Sprintf("pkg/use%d.go", i)
		caller := &graph.Node{
			ID: fmt.Sprintf("%s::Use%d", file, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("Use%d", i), FilePath: file, StartLine: 3,
		}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5})
	}
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil), hot.ID
}

type usagesLimitResponse struct {
	Edges        []*graph.Edge       `json:"edges"`
	Nodes        []json.RawMessage   `json:"nodes"`
	TotalEdges   int                 `json:"total_edges"`
	Truncated    bool                `json:"truncated"`
	UsageSummary *query.UsageSummary `json:"usage_summary"`
}

// TestFindUsages_LimitCapsUsageRows pins the advertised `limit` option:
// a call asking for 2 rows gets exactly 2, marked truncated, with the
// full row count still legible on total_edges and the completeness
// rollup still covering the whole usage set.
func TestFindUsages_LimitCapsUsageRows(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 6)

	var resp usagesLimitResponse
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 2})), &resp))
	require.Len(t, resp.Edges, 2, "limit:2 must return exactly 2 usage rows")
	require.True(t, resp.Truncated, "a capped response must be marked truncated")
	require.Equal(t, 6, resp.TotalEdges, "total_edges must keep the full row count")
	require.NotNil(t, resp.UsageSummary)
	require.Equal(t, 6, resp.UsageSummary.NRefs, "the completeness rollup must describe the full set, not the page")
}

// TestFindUsages_DefaultLimitApplies pins the schema's "default: 50":
// with no limit argument a 55-caller symbol answers with 50 rows and a
// truncation marker instead of the whole set.
func TestFindUsages_DefaultLimitApplies(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 55)

	var resp usagesLimitResponse
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": hotID})), &resp))
	require.Len(t, resp.Edges, 50, "the advertised default limit is 50")
	require.True(t, resp.Truncated)
	require.Equal(t, 55, resp.TotalEdges)
}

// TestFindUsages_LimitOptOut pins limit:0 as the explicit no-cap
// escape hatch, mirroring the max_bytes/max_tokens opt-out semantics.
func TestFindUsages_LimitOptOut(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 55)

	var resp usagesLimitResponse
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 0})), &resp))
	require.Len(t, resp.Edges, 55, "limit:0 opts out of the cap")
	require.False(t, resp.Truncated)
}

// TestFindUsages_LimitTruncationMetaGCX pins the truncation indicator
// on the GCX wire: a capped response carries truncated=true plus the
// full total, an uncapped response keeps its wire shape unchanged.
func TestFindUsages_LimitTruncationMetaGCX(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 6)

	out := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "gcx", "limit": 2})
	require.Contains(t, out, "truncated=true")
	require.Contains(t, out, "total_edges=6")

	full := findUsagesText(t, srv, map[string]any{"id": hotID, "format": "gcx", "limit": 0})
	require.NotContains(t, full, "truncated=true", "an uncapped response must not carry truncation meta")
}

// TestFindUsages_LimitKeepsStrongestEvidence pins the order the cap
// consumes: rows are ranked by evidence tier before the page is cut, so
// a weak ast_inferred row inserted earlier can never evict an
// lsp_resolved usage from the page.
func TestFindUsages_LimitKeepsStrongestEvidence(t *testing.T) {
	g := graph.New()
	hot := &graph.Node{ID: "pkg/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "pkg/hot.go", StartLine: 1}
	g.AddNode(hot)
	// Weak rows first in insertion order, strong rows last.
	for i := 0; i < 4; i++ {
		file := fmt.Sprintf("pkg/weak%d.go", i)
		caller := &graph.Node{ID: file + "::Weak", Kind: graph.KindFunction, Name: "Weak", FilePath: file, StartLine: 3}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5, Origin: graph.OriginASTInferred, Confidence: 0.6})
	}
	for i := 0; i < 2; i++ {
		file := fmt.Sprintf("pkg/strong%d.go", i)
		caller := &graph.Node{ID: file + "::Strong", Kind: graph.KindFunction, Name: "Strong", FilePath: file, StartLine: 3}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5, Origin: graph.OriginLSPResolved, Confidence: 1.0})
	}
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	var resp usagesLimitResponse
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": hot.ID, "limit": 2})), &resp))
	require.Len(t, resp.Edges, 2)
	for _, e := range resp.Edges {
		require.Equal(t, graph.OriginLSPResolved, e.Origin,
			"the capped page must keep the strongest evidence, not the earliest-inserted rows")
	}
}

// TestFindUsages_LimitPageBackendParity pins that the capped page is
// identical on the in-memory and SQLite backends. The two stores
// iterate edges in different orders (insertion vs kind/id), so without
// one global sort before the cut the default page depends on which
// backend served it.
func TestFindUsages_LimitPageBackendParity(t *testing.T) {
	build := func(g graph.Store) string {
		hot := &graph.Node{ID: "pkg/hot.go::Hot", Kind: graph.KindFunction, Name: "Hot", FilePath: "pkg/hot.go", StartLine: 1}
		g.AddNode(hot)
		// Mixed edge kinds in descending file order: the memory store
		// iterates in-edges by insertion, SQLite groups them by kind, so
		// without one global sort the two pages diverge.
		kinds := []graph.EdgeKind{graph.EdgeReferences, graph.EdgeCalls, graph.EdgeImports, graph.EdgeInstantiates}
		for i := 9; i >= 0; i-- {
			file := fmt.Sprintf("pkg/use%d.go", i)
			caller := &graph.Node{ID: fmt.Sprintf("%s::Use%d", file, i), Kind: graph.KindFunction, Name: fmt.Sprintf("Use%d", i), FilePath: file, StartLine: 3}
			g.AddNode(caller)
			g.AddEdge(&graph.Edge{From: caller.ID, To: hot.ID, Kind: kinds[i%len(kinds)], FilePath: file, Line: 5, Origin: graph.OriginASTResolved, Confidence: 0.9})
		}
		return hot.ID
	}
	page := func(g graph.Store, hotID string) []string {
		eng := query.NewEngine(g)
		eng.SetSearch(search.NewNull())
		srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
		var resp usagesLimitResponse
		require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 3})), &resp))
		froms := make([]string, 0, len(resp.Edges))
		for _, e := range resp.Edges {
			froms = append(froms, e.From)
		}
		return froms
	}

	mem := graph.New()
	memID := build(mem)

	sqlite, err := store_sqlite.Open(filepath.Join(t.TempDir(), "usages.sqlite"))
	require.NoError(t, err)
	defer sqlite.Close()
	sqliteID := build(sqlite)

	memPage := page(mem, memID)
	sqlitePage := page(sqlite, sqliteID)
	require.Len(t, memPage, 3)
	require.Equal(t, memPage, sqlitePage, "the capped page must not depend on the store backend")
}

// TestFindUsages_LimitOrdersReExportUnion pins the stable order on the
// barrel re-export union path: the page over facade + canonical usages
// is cut after the merged set is sorted, so the strongest canonical
// rows win over the facade's weaker direct refs regardless of merge
// order, and the totals describe the whole union.
func TestFindUsages_LimitOrdersReExportUnion(t *testing.T) {
	g := graph.New()
	facade := &graph.Node{
		ID: "src/index.ts::persist", Kind: graph.KindFunction, Name: "persist",
		FilePath: "src/index.ts", StartLine: 1, Meta: map[string]any{"reexport": true},
	}
	canon := &graph.Node{ID: "src/middleware.ts::persist", Kind: graph.KindFunction, Name: "persist", FilePath: "src/middleware.ts", StartLine: 10}
	g.AddNode(facade)
	g.AddNode(canon)
	g.AddEdge(&graph.Edge{From: facade.ID, To: canon.ID, Kind: graph.EdgeReExports, FilePath: "src/index.ts", Line: 1})
	// Facade's own direct refs are weak; the canonical's usages are
	// lsp_resolved. The merge appends the canonical set second, so an
	// unsorted cut would keep the weak facade rows.
	for i := 0; i < 2; i++ {
		file := fmt.Sprintf("src/facadeuse%d.ts", i)
		caller := &graph.Node{ID: file + "::useFacade", Kind: graph.KindFunction, Name: "useFacade", FilePath: file, StartLine: 3}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: facade.ID, Kind: graph.EdgeImports, FilePath: file, Line: 1, Origin: graph.OriginASTInferred, Confidence: 0.6})
	}
	for i := 0; i < 4; i++ {
		file := fmt.Sprintf("src/canonuse%d.ts", i)
		caller := &graph.Node{ID: file + "::useCanon", Kind: graph.KindFunction, Name: "useCanon", FilePath: file, StartLine: 3}
		g.AddNode(caller)
		g.AddEdge(&graph.Edge{From: caller.ID, To: canon.ID, Kind: graph.EdgeCalls, FilePath: file, Line: 5, Origin: graph.OriginLSPResolved, Confidence: 1.0})
	}
	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	var resp usagesLimitResponse
	require.NoError(t, json.Unmarshal([]byte(findUsagesText(t, srv, map[string]any{"id": facade.ID, "limit": 3})), &resp))
	require.Len(t, resp.Edges, 3)
	for _, e := range resp.Edges {
		require.Equal(t, graph.OriginLSPResolved, e.Origin,
			"the union page must keep the strongest rows across facade + canonical")
	}
	require.True(t, resp.Truncated)
	// 2 facade imports + 1 re-export edge is not a usage of the facade
	// per se; the union total counts the merged usage rows.
	require.Equal(t, resp.TotalEdges, 6+1, "total_edges covers the merged union")
}

// TestFindUsages_GroupByFileHonorsLimit pins the row cap on the
// group_by:"file" shape: the buckets cover the capped page, and a
// truncated grouped response carries the full total alongside its
// per-page counts so the cut stays legible.
func TestFindUsages_GroupByFileHonorsLimit(t *testing.T) {
	srv, hotID := usagesLimitServer(t, 6)

	var resp struct {
		TotalUses  int  `json:"total_uses"`
		TotalEdges int  `json:"total_edges"`
		Truncated  bool `json:"truncated"`
	}
	out := findUsagesText(t, srv, map[string]any{"id": hotID, "limit": 2, "group_by": "file"})
	require.NoError(t, json.Unmarshal([]byte(out), &resp))
	require.Equal(t, 2, resp.TotalUses, "the grouped page covers the capped rows")
	require.True(t, resp.Truncated)
	require.Equal(t, 6, resp.TotalEdges, "a truncated grouped page must carry the full total")
}

// TestFindUsages_CompactWinsOverGCX pins the `compact` option against
// the GCX format path: compact is an explicit caller choice, so it
// takes precedence exactly as it does in the shared returnSubGraph
// renderer — including for sessions whose default format is gcx, where
// isGCX(ctx, req) is true without any format argument. The explicit
// format:"gcx" arg reproduces that same isGCX=true condition without
// needing a session handshake.
func TestFindUsages_CompactWinsOverGCX(t *testing.T) {
	srv, fooID, _ := usagesSummaryServer(t)

	out := findUsagesText(t, srv, map[string]any{"id": fooID, "format": "gcx", "compact": true})
	require.Contains(t, out, "edges: 4 total", "compact:true must render the one-line-per-symbol text format")
	require.Contains(t, out, "function Use1 pkg/a.go:3", "compact rows carry kind, name, and location")
	require.NotContains(t, out, "from_is_test", "compact output must not be the GCX row encoding")
}
