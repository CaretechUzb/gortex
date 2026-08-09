package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/analysis"
	"github.com/zzet/gortex/internal/graph"
)

// newImpactTestServer builds a server whose graph has one clear
// blast-radius hub (`hub`) plus a set of leaf callers.
func newImpactTestServer(t *testing.T) *Server {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "core.go::Hub", Kind: graph.KindFunction, Name: "Hub",
		FilePath: "core.go", StartLine: 10,
		Meta: map[string]any{"complexity": 12},
	})
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		g.AddNode(&graph.Node{
			ID: "callers.go::" + id, Kind: graph.KindFunction, Name: id,
			FilePath: "callers.go", StartLine: 1,
		})
		g.AddEdge(&graph.Edge{From: "callers.go::" + id, To: "core.go::Hub", Kind: graph.EdgeCalls})
	}
	// An island in another community: nothing reaches it from the hub, so
	// a target-scoped call must never rank it.
	g.AddNode(&graph.Node{
		ID: "island.go::Island", Kind: graph.KindFunction, Name: "Island",
		FilePath: "island.go", StartLine: 1,
	})

	s := &Server{
		graph:      g,
		session:    newSessionState(),
		tokenStats: &tokenStats{},
		symHistory: &symbolHistory{entries: make(map[string][]SymbolModification)},
		sessions:   newSessionMap(),
		toolScopes: newScopeRegistry(),
	}
	s.analysisMu.Lock()
	s.pageRank = analysis.ComputePageRank(g)
	s.analysisMu.Unlock()
	installCommunitiesForTest(s, analysis.DetectCommunities(g))
	// Consume the co-change once-guard so ensureCoChange does not
	// shell out to git during the test.
	s.cochangeOnce.Do(func() {})
	return s
}

func callAnalyzeImpact(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleAnalyzeImpactComposite(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &m))
	return m
}

// rawAnalyzeImpact keeps the error result so fail-closed paths are
// assertable; callAnalyzeImpact above insists on success.
func rawAnalyzeImpact(t *testing.T, s *Server, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleAnalyzeImpactComposite(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	return res
}

func impactRowIDs(t *testing.T, out map[string]any) []string {
	t.Helper()
	symbols, _ := out["symbols"].([]any)
	ids := make([]string, 0, len(symbols))
	for _, raw := range symbols {
		row, _ := raw.(map[string]any)
		id, _ := row["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

func TestAnalyzeImpact_HubRanksAboveLeaves(t *testing.T) {
	s := newImpactTestServer(t)
	out := callAnalyzeImpact(t, s, map[string]any{})

	symbols, _ := out["symbols"].([]any)
	require.NotEmpty(t, symbols)
	// The hub — five callers, complexity 12 — outranks every leaf.
	first, _ := symbols[0].(map[string]any)
	require.Equal(t, "core.go::Hub", first["id"])

	score, _ := first["score"].(float64)
	require.Greater(t, score, 0.0)
	// Per-axis breakdown is present and explainable.
	require.Contains(t, first, "centrality")
	require.Contains(t, first, "reach")
	require.Contains(t, first, "complexity")
	require.Contains(t, first, "co_change")
	require.Contains(t, first, "community")
	require.Equal(t, float64(5), first["fan_in"])
	require.Equal(t, float64(12), first["cyclomatic"])
}

func TestAnalyzeImpact_IDsFilter(t *testing.T) {
	s := newImpactTestServer(t)
	out := callAnalyzeImpact(t, s, map[string]any{"ids": "core.go::Hub"})
	symbols, _ := out["symbols"].([]any)
	require.Len(t, symbols, 1)
	first, _ := symbols[0].(map[string]any)
	require.Equal(t, "core.go::Hub", first["id"])
}

func TestAnalyzeImpact_WeightsInResponse(t *testing.T) {
	s := newImpactTestServer(t)
	out := callAnalyzeImpact(t, s, map[string]any{})
	weights, ok := out["weights"].(map[string]any)
	require.True(t, ok)
	for _, axis := range []string{"centrality", "reach", "complexity", "co_change", "community"} {
		require.Contains(t, weights, axis)
	}
}

func TestAnalyzeImpact_MinScoreFilter(t *testing.T) {
	s := newImpactTestServer(t)
	// A threshold above every leaf's score keeps only the hub.
	out := callAnalyzeImpact(t, s, map[string]any{"min_score": 99.0})
	symbols, _ := out["symbols"].([]any)
	require.Empty(t, symbols)
}

// A target-scoped call answers "what breaks if I change X": the target
// leads its own result and only its dependents follow it. Before this,
// the target was dropped and the caller got a repo-wide hotspot ranking
// that looked authoritative.
func TestAnalyzeImpact_TargetRanksItsOwnClosure(t *testing.T) {
	s := newImpactTestServer(t)
	out := callAnalyzeImpact(t, s, map[string]any{"id": "core.go::Hub"})

	require.Equal(t, "target_closure", out["scope"])
	ids := impactRowIDs(t, out)
	require.Equal(t, "core.go::Hub", ids[0], "the target leads its own blast radius")
	require.NotContains(t, ids, "island.go::Island", "an unreachable symbol is not blast radius")
	require.Len(t, ids, 6, "the target plus its five callers")

	symbols, _ := out["symbols"].([]any)
	first, _ := symbols[0].(map[string]any)
	require.Equal(t, true, first["target"])
	for _, raw := range symbols[1:] {
		row, _ := raw.(map[string]any)
		require.Equal(t, float64(1), row["depth"], "direct callers sit at depth 1")
	}

	target, ok := out["target"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "core.go::Hub", target["symbol"])
	require.Equal(t, float64(5), target["dependents"])
	byDepth, _ := target["dependents_by_depth"].(map[string]any)
	require.Equal(t, float64(5), byDepth["1"])
}

// The reported call shape: analyze(kind:"impact", target:{symbol:…}).
// This calls the lowering and the handler directly, so it proves only that
// applyFacadeTarget maps target.symbol to id. It passed throughout the period
// when no real call could reach either — the argument reconciler renamed the
// selector away before dispatch, and the reused legacy name never routed
// through the facade outside a facade-v1 session. The protocol-level guard
// against that is TestAnalyzeTargetReachesHandlerOnEverySurface.
func TestAnalyzeImpact_FacadeTargetReachesTheHandler(t *testing.T) {
	s := newImpactTestServer(t)
	spec := facadeOperationSpec{Facade: "analyze", Operation: "impact", Legacy: "analyze"}
	args := normalizeFacadeArguments(spec, map[string]any{
		"kind":   "impact",
		"target": map[string]any{"symbol": "core.go::Hub"},
	})
	require.Equal(t, "core.go::Hub", args["id"], "the facade lowers target.symbol to id")

	req := mcp.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &out))
	require.Equal(t, "target_closure", out["scope"])
	require.Contains(t, impactRowIDs(t, out), "core.go::Hub")
}

// An unresolvable target fails closed. Falling back to the repo-wide
// ranking would hand the caller a confident answer to another question.
func TestAnalyzeImpact_UnknownTargetFailsClosed(t *testing.T) {
	s := newImpactTestServer(t)
	res := rawAnalyzeImpact(t, s, map[string]any{"id": "core.go::Nope"})
	require.True(t, res.IsError)
	tc, ok := res.Content[0].(mcp.TextContent)
	require.True(t, ok)
	var structured map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &structured))
	require.Equal(t, string(ErrCodeSymbolNotFound), structured["error_code"])
	require.NotContains(t, tc.Text, "island.go::Island")
}

func TestAnalyzeImpact_UnindexedFileTargetFailsClosed(t *testing.T) {
	s := newImpactTestServer(t)
	res := rawAnalyzeImpact(t, s, map[string]any{"path": "nowhere.go"})
	require.True(t, res.IsError)
	tc, _ := res.Content[0].(mcp.TextContent)
	var structured map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &structured))
	require.Equal(t, string(ErrCodeFileNotIndexed), structured["error_code"])
}

// A file target seeds on every symbol the file defines.
func TestAnalyzeImpact_FileTargetScopesToItsSymbols(t *testing.T) {
	s := newImpactTestServer(t)
	out := callAnalyzeImpact(t, s, map[string]any{"path": "callers.go"})
	ids := impactRowIDs(t, out)
	require.Len(t, ids, 5)
	require.NotContains(t, ids, "island.go::Island")
	target, _ := out["target"].(map[string]any)
	require.Equal(t, "callers.go", target["file"])
	require.Equal(t, float64(0), target["dependents"])
}

// Zero dependents and an extraction gap look identical from the outside,
// so an empty closure must say so rather than read as "safe to change".
func TestAnalyzeImpact_LeafTargetCarriesZeroDependentsCaveat(t *testing.T) {
	s := newImpactTestServer(t)
	out := callAnalyzeImpact(t, s, map[string]any{"id": "callers.go::a"})
	require.Equal(t, []string{"callers.go::a"}, impactRowIDs(t, out))
	target, _ := out["target"].(map[string]any)
	require.Equal(t, float64(0), target["dependents"])
	require.Equal(t, zeroDependentsCaveat, target["zero_dependents_caveat"])
}

// Filters narrow the dependents; they never drop the symbol the question
// is about.
func TestAnalyzeImpact_FiltersNeverDropTheTarget(t *testing.T) {
	s := newImpactTestServer(t)
	out := callAnalyzeImpact(t, s, map[string]any{"id": "core.go::Hub", "min_score": 99.0})
	require.Equal(t, []string{"core.go::Hub"}, impactRowIDs(t, out))
}

// Without a target the repo-wide ranking keeps its old shape.
func TestAnalyzeImpact_NoTargetKeepsRepoWideRanking(t *testing.T) {
	s := newImpactTestServer(t)
	out := callAnalyzeImpact(t, s, map[string]any{})
	require.NotContains(t, out, "scope")
	require.NotContains(t, out, "target")
	require.Contains(t, impactRowIDs(t, out), "island.go::Island")
}

func TestAnalyzeImpact_FacadeSchemaDeclaresOptionalTarget(t *testing.T) {
	spec := facadeOperationSpec{Facade: "analyze", Operation: "impact", Legacy: "analyze"}
	schema, _, _ := analyzeFacadeCapabilitySchema(spec, map[string]any{}, nil)
	properties, _ := schema["properties"].(map[string]any)
	target, ok := properties["target"].(map[string]any)
	require.True(t, ok, "analyze.impact must declare its target selector")
	targetProperties, _ := target["properties"].(map[string]any)
	require.Contains(t, targetProperties, "symbol")
	require.Contains(t, targetProperties, "file")
	required, _ := schema["required"].([]string)
	require.NotContains(t, required, "target", "impact's target stays optional")
}

func TestAnalyzeImpact_DispatchedViaAnalyze(t *testing.T) {
	s := newImpactTestServer(t)
	req := mcp.CallToolRequest{}
	req.Params.Arguments = map[string]any{"kind": "impact"}
	res, err := s.handleAnalyze(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.IsError)
}
