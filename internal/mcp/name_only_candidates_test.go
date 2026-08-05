package mcp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// unboundCallSitesGraph reproduces the reported shape end to end: a
// function with three real call sites, one of which the resolver bound.
// The other two sit on the bare-name placeholder — the state the
// cross-package guard leaves behind when it reverts a weak-tier guess —
// so no reverse walk over the target's in-edges can see them.
func unboundCallSitesGraph() *graph.Graph {
	g := graph.New()
	for _, f := range []string{"src/data/Techs.gd", "src/data/Codex.gd", "src/systems/Research.gd", "src/ui/Main.gd"} {
		g.AddNode(&graph.Node{ID: f, Kind: graph.KindFile, Name: f, FilePath: f, Language: "gdscript"})
	}
	g.AddNode(&graph.Node{
		ID: "src/data/Techs.gd::get_tech", Kind: graph.KindFunction, Name: "get_tech",
		FilePath: "src/data/Techs.gd", Language: "gdscript",
	})
	g.AddEdge(&graph.Edge{From: "src/data/Techs.gd", To: "src/data/Techs.gd::get_tech", Kind: graph.EdgeDefines})

	// The one caller the resolver bound — it lives in the target's own
	// directory, which is the only thing the locality fallback could use.
	g.AddNode(&graph.Node{
		ID: "src/data/Codex.gd::_tech_articles", Kind: graph.KindFunction, Name: "_tech_articles",
		FilePath: "src/data/Codex.gd", Language: "gdscript",
	})
	g.AddEdge(&graph.Edge{
		From: "src/data/Codex.gd::_tech_articles", To: "src/data/Techs.gd::get_tech",
		Kind: graph.EdgeCalls, FilePath: "src/data/Codex.gd", Line: 31,
		Origin: graph.OriginASTResolved, Confidence: 0.9,
	})

	// Two real call sites left on the placeholder.
	for _, c := range []struct {
		file, fn string
		line     int
	}{
		{"src/systems/Research.gd", "advance", 88},
		{"src/ui/Main.gd", "_refresh", 204},
	} {
		g.AddNode(&graph.Node{
			ID: c.file + "::" + c.fn, Kind: graph.KindFunction, Name: c.fn,
			FilePath: c.file, Language: "gdscript",
		})
		g.AddEdge(&graph.Edge{
			From: c.file + "::" + c.fn, To: "unresolved::*.get_tech",
			Kind: graph.EdgeCalls, FilePath: c.file, Line: c.line,
		})
	}
	return g
}

// The default answer must not pretend the unbound sites do not exist.
// Reporting one caller with no hint that two more name the symbol is what
// let change_impact answer "risk: LOW" for a change that breaks three
// files — issue #465.
func TestGetCallers_ReportsNameOnlyCandidateCount(t *testing.T) {
	srv := serverForGraph(t, unboundCallSitesGraph())

	res := decodeJSONResult(t, callTool(t, srv, "get_callers", map[string]any{
		"id": "src/data/Techs.gd::get_tech",
	}))

	edges, _ := res["edges"].([]any)
	assert.Len(t, edges, 1, "the default answer stays resolver-verified")
	assert.EqualValues(t, 2, res["name_only_candidates"],
		"the unbound call sites must be counted even when their rows are withheld")
}

// The explicit opt-in returns the rows themselves, so an agent that has
// decided name-matched evidence is worth having can actually get it.
// Before this, no value of min_tier could surface them.
func TestGetCallers_MinTierTextMatchedReturnsNameOnlyRows(t *testing.T) {
	srv := serverForGraph(t, unboundCallSitesGraph())

	res := decodeJSONResult(t, callTool(t, srv, "get_callers", map[string]any{
		"id":       "src/data/Techs.gd::get_tech",
		"min_tier": graph.OriginTextMatched,
	}))

	edges, _ := res["edges"].([]any)
	require.Len(t, edges, 3, "min_tier:text_matched must add the two unbound call sites")
	assert.EqualValues(t, 2, res["name_only_candidates"])

	// Every added row is labelled for what it is on the wire — a consumer
	// must be able to tell a never-bound candidate from a text_matched
	// edge the resolver actually chose.
	nameOnly := 0
	for _, raw := range edges {
		e, _ := raw.(map[string]any)
		if e["name_only"] != true {
			continue
		}
		nameOnly++
		assert.Equal(t, graph.OriginTextMatched, e["origin"])
		assert.Equal(t, "src/data/Techs.gd::get_tech", e["to"],
			"a candidate row points at the queried symbol so it reads as a caller")
	}
	assert.Equal(t, 2, nameOnly)

	// The resolver-bound caller must NOT be flagged.
	for _, raw := range edges {
		e, _ := raw.(map[string]any)
		if e["from"] == "src/data/Codex.gd::_tech_articles" {
			assert.Nil(t, e["name_only"], "a resolved caller carries no candidate marker")
		}
	}
}

// find_usages shares the surface — the issue's `relations callers` and a
// usage query must not disagree about how much the graph does not know.
func TestFindUsages_ReportsNameOnlyCandidateCount(t *testing.T) {
	srv := serverForGraph(t, unboundCallSitesGraph())

	res := decodeJSONResult(t, callTool(t, srv, "find_usages", map[string]any{
		"id": "src/data/Techs.gd::get_tech",
	}))
	assert.EqualValues(t, 2, res["name_only_candidates"])

	opted := decodeJSONResult(t, callTool(t, srv, "find_usages", map[string]any{
		"id":       "src/data/Techs.gd::get_tech",
		"min_tier": graph.OriginTextMatched,
	}))
	edges, _ := opted["edges"].([]any)
	assert.Len(t, edges, 3)
}

// A symbol whose calls all resolved must stay clean: the field is omitted
// entirely, so it never becomes background noise on healthy answers.
func TestGetCallers_NoCandidatesWhenEverythingResolved(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a.go", To: "a.go::Target", Kind: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{
		From: "a.go::Caller", To: "a.go::Target", Kind: graph.EdgeCalls,
		FilePath: "a.go", Line: 9, Origin: graph.OriginLSPResolved, Confidence: 1,
	})

	res := decodeJSONResult(t, callTool(t, serverForGraph(t, g), "get_callers", map[string]any{"id": "a.go::Target"}))
	_, present := res["name_only_candidates"]
	assert.False(t, present, "a fully-resolved symbol must not carry the field at all")
}
