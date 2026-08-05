package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// weakOnlyGraph gives `a.go::Target` two callers, both bound by bare-name
// matching alone — the shape a common method name (`Get`, `Run`, `Close`)
// produces. Nothing corroborates either edge, so the populated result is
// not evidence the symbol is used.
func weakOnlyGraph() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::Weak1", Kind: graph.KindFunction, Name: "Weak1", FilePath: "b.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b.go::Weak2", Kind: graph.KindFunction, Name: "Weak2", FilePath: "b.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a.go", To: "a.go::Target", Kind: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: "b.go::Weak1", To: "a.go::Target", Kind: graph.EdgeCalls,
		FilePath: "b.go", Line: 5, Origin: graph.OriginTextMatched})
	g.AddEdge(&graph.Edge{From: "b.go::Weak2", To: "a.go::Target", Kind: graph.EdgeCalls,
		FilePath: "b.go", Line: 7, Origin: graph.OriginTextMatched})
	return g
}

// addStrongCaller adds a third caller bound by compiler-grade evidence, so
// the same fixture can assert the caveat disappears. It must be a distinct
// caller node: get_callers collapses multiple edges from one caller, so
// re-binding an existing weak caller would be deduped away.
func addStrongCaller(g *graph.Graph) {
	g.AddNode(&graph.Node{ID: "c.go", Kind: graph.KindFile, Name: "c.go", FilePath: "c.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "c.go::Strong", Kind: graph.KindFunction, Name: "Strong", FilePath: "c.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "c.go::Strong", To: "a.go::Target", Kind: graph.EdgeCalls,
		FilePath: "c.go", Line: 42, Origin: graph.OriginLSPResolved, Confidence: 1})
}

// A populated result whose every row is a name-only match must say so.
// Without the caveat the noise reads as proof the symbol is live, which is
// exactly how genuine dead code hides behind a common name — issue #433.
func TestFindUsages_WeakEvidenceOnlyCarriesCaveat(t *testing.T) {
	g := weakOnlyGraph()
	srv := serverForGraph(t, g)

	res := decodeJSONResult(t, callTool(t, srv, "find_usages", map[string]any{"id": "a.go::Target"}))
	edges, _ := res["edges"].([]any)
	require.NotEmpty(t, edges, "fixture: the weak callers must survive to the response")

	caveat, ok := res["caveat"].(map[string]any)
	require.True(t, ok, "find_usages must caveat a result made entirely of name-only matches")
	assert.Equal(t, string(graph.ZeroEdgeCoverageIncomplete), caveat["class"])
	assert.NotEmpty(t, caveat["message"])

	// One compiler-grade caller and the caveat must go — otherwise every
	// common-named symbol with real callers collects a spurious warning.
	addStrongCaller(g)
	strong := decodeJSONResult(t, callTool(t, serverForGraph(t, g), "find_usages", map[string]any{"id": "a.go::Target"}))
	_, hasCaveat := strong["caveat"]
	assert.False(t, hasCaveat, "a result with one trustworthy usage must omit the caveat")
}

func TestGetCallers_WeakEvidenceOnlyCarriesCaveat(t *testing.T) {
	g := weakOnlyGraph()
	srv := serverForGraph(t, g)

	res := decodeJSONResult(t, callTool(t, srv, "get_callers", map[string]any{"id": "a.go::Target"}))
	edges, _ := res["edges"].([]any)
	require.NotEmpty(t, edges, "fixture: the weak callers must survive to the response")

	caveat, ok := res["caveat"].(map[string]any)
	require.True(t, ok, "get_callers must caveat a caller list made entirely of name-only matches")
	assert.Equal(t, string(graph.ZeroEdgeCoverageIncomplete), caveat["class"])
	assert.NotEmpty(t, caveat["message"])

	addStrongCaller(g)
	strong := decodeJSONResult(t, callTool(t, serverForGraph(t, g), "get_callers", map[string]any{"id": "a.go::Target"}))
	_, hasCaveat := strong["caveat"]
	assert.False(t, hasCaveat, "a caller list with one trustworthy caller must omit the caveat")
}

// An emptiness the caller's own min_tier produced is already explained by
// tier_filtered. Classifying on top would double-report it — and would
// name the very tier the caller asked to exclude — so get_callers must
// stay silent there, matching find_usages.
func TestGetCallers_MinTierEmptinessIsNotCaveated(t *testing.T) {
	srv := serverForGraph(t, weakOnlyGraph())

	res := decodeJSONResult(t, callTool(t, srv, "get_callers", map[string]any{
		"id":       "a.go::Target",
		"min_tier": graph.OriginASTResolved,
	}))
	edges, _ := res["edges"].([]any)
	require.Empty(t, edges, "fixture: min_tier ast_resolved must drop both text_matched callers")

	_, hasTierFiltered := res["tier_filtered"]
	assert.True(t, hasTierFiltered, "the min_tier filter must record why the result emptied")
	_, hasCaveat := res["caveat"]
	assert.False(t, hasCaveat, "a min_tier emptiness must not also be classified as a coverage gap")
}

// A speculative edge is hidden from the response by default, so counting it
// as proof of use turns an empty answer into an *uncaveated* empty answer —
// the disarmed-safety-gate case this classification exists to prevent.
func TestFindUsages_SpeculativeOnlyEmptyResultIsCaveated(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a.go", To: "a.go::Target", Kind: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: "a.go::Caller", To: "a.go::Target", Kind: graph.EdgeCalls,
		FilePath: "a.go", Line: 9, Origin: graph.OriginASTResolved, Confidence: 0.9,
		Meta: map[string]any{graph.MetaSpeculative: true}})

	res := decodeJSONResult(t, callTool(t, serverForGraph(t, g), "find_usages", map[string]any{"id": "a.go::Target"}))
	edges, _ := res["edges"].([]any)
	require.Empty(t, edges, "fixture: a speculative edge is excluded from the default response")

	caveat, ok := res["caveat"].(map[string]any)
	require.True(t, ok, "an empty result must not be left uncaveated because a hidden edge exists")
	assert.Equal(t, string(graph.ZeroEdgeCoverageIncomplete), caveat["class"])
}
