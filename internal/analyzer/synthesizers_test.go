package analyzer_test

// PURPOSE — shape tests for AnalyzeSynthesizers: verify the function
// returns the correct JSON-matching struct given a graph with synthesized
// edges.
// RATIONALE — these tests live here rather than in mcp/ so the core logic
// is independently verifiable without the MCP layer.
// KEYWORDS — synthesizers, unit, shape

import (
	"testing"

	"github.com/zzet/gortex/internal/analyzer"
	"github.com/zzet/gortex/internal/graph"
)

func newTestGraph() graph.Store {
	return graph.New()
}

func addSynthEdge(g graph.Store, from, to, by, via string) {
	g.AddEdge(&graph.Edge{
		From: from, To: to, Kind: graph.EdgeCalls,
		Meta: map[string]any{
			"synthesized_by": by,
			"provenance":     "heuristic",
			"via":            via,
		},
	})
}

func TestAnalyzeSynthesizers_Shape(t *testing.T) {
	g := newTestGraph()
	addSynthEdge(g, "a.go::A", "b.go::B", "event-channel", "event.channel")
	addSynthEdge(g, "a.go::A", "c.go::C", "event-channel", "event.channel")
	addSynthEdge(g, "cli.go::run", "svc.go::Handle", "grpc-stub", "grpc.stub")

	res := mustAnalyzeSynthesizers(t, g)
	if res.TotalEdges != 3 {
		t.Fatalf("expected TotalEdges=3, got %d", res.TotalEdges)
	}
	if len(res.Synthesizers) != 2 {
		t.Fatalf("expected 2 synthesizer groups, got %d", len(res.Synthesizers))
	}
	// Sorted by edges desc: event-channel first.
	first := res.Synthesizers[0]
	if first.Name != "event-channel" {
		t.Errorf("expected event-channel first, got %q", first.Name)
	}
	if first.Edges != 2 {
		t.Errorf("expected 2 edges for event-channel, got %d", first.Edges)
	}
	if first.Provenance != "heuristic" {
		t.Errorf("expected heuristic provenance, got %q", first.Provenance)
	}
}

func TestAnalyzeSynthesizers_NameFilter(t *testing.T) {
	g := newTestGraph()
	addSynthEdge(g, "a.go::A", "b.go::B", "event-channel", "event.channel")
	addSynthEdge(g, "cli.go::run", "svc.go::Handle", "grpc-stub", "grpc.stub")

	res := mustAnalyzeSynthesizers(t, g, analyzer.WithSynthesizerNameFilter("grpc-stub"))
	if len(res.Synthesizers) != 1 {
		t.Fatalf("expected 1 group with name filter, got %d", len(res.Synthesizers))
	}
	if res.Synthesizers[0].Name != "grpc-stub" {
		t.Errorf("name filter failed: %q", res.Synthesizers[0].Name)
	}
}

func TestAnalyzeSynthesizers_NoSynthEdges(t *testing.T) {
	g := newTestGraph()
	// plain non-synthesized edge
	g.AddEdge(&graph.Edge{From: "x.go::X", To: "y.go::Y", Kind: graph.EdgeCalls})

	res := mustAnalyzeSynthesizers(t, g)
	if res.TotalEdges != 0 {
		t.Fatalf("expected 0 total_edges, got %d", res.TotalEdges)
	}
	if len(res.Synthesizers) != 0 {
		t.Fatalf("expected no synthesizer groups, got %d", len(res.Synthesizers))
	}
}
