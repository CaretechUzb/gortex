package mcp

import (
	"strconv"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestAuditScoreGrade_Boundaries(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{100, "A"}, {85.01, "A"}, {85, "A"}, {84.99, "B"}, {70, "B"},
		{69.99, "C"}, {55, "C"}, {40, "D"}, {39.99, "F"}, {0, "F"}, {-1, "F"},
	}
	for _, c := range cases {
		if got := auditScoreGrade(c.score); got != c.want {
			t.Errorf("auditScoreGrade(%.2f) = %q, want %q", c.score, got, c.want)
		}
	}
}

func TestComputeAuditReport_EmptyGraphReturnsF(t *testing.T) {
	emptyG := graph.New()
	r := ComputeAuditReport(emptyG, emptyG.AllNodes())
	if r.SymbolCount != 0 {
		t.Errorf("empty graph SymbolCount = %d, want 0", r.SymbolCount)
	}
	if r.Grade != "F" {
		t.Errorf("empty graph Grade = %q, want F", r.Grade)
	}
}

func TestComputeAuditReport_ScoresAndGradesPopulated(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "f.go::low", Name: "low", Kind: graph.KindFunction, FilePath: "f.go", StartLine: 1})
	g.AddNode(&graph.Node{ID: "f.go::med", Name: "med", Kind: graph.KindFunction, FilePath: "f.go", StartLine: 10})
	g.AddNode(&graph.Node{ID: "f.go::hot", Name: "hot", Kind: graph.KindFunction, FilePath: "f.go", StartLine: 20})
	for i := range 10 {
		g.AddNode(&graph.Node{ID: "c.go::caller_" + strconv.Itoa(i), Kind: graph.KindFunction})
		g.AddEdge(&graph.Edge{From: "c.go::caller_" + strconv.Itoa(i), To: "f.go::hot", Kind: graph.EdgeCalls})
	}
	for i := range 5 {
		g.AddNode(&graph.Node{ID: "callee.go::callee_" + strconv.Itoa(i), Kind: graph.KindFunction})
		g.AddEdge(&graph.Edge{From: "f.go::hot", To: "callee.go::callee_" + strconv.Itoa(i), Kind: graph.EdgeCalls})
	}
	for i := range 3 {
		g.AddEdge(&graph.Edge{From: "c.go::caller_" + strconv.Itoa(i), To: "f.go::med", Kind: graph.EdgeCalls})
	}

	r := ComputeAuditReport(g, g.AllNodes())
	if r.SymbolCount == 0 {
		t.Fatalf("expected callable symbols in report, got 0")
	}
	if r.MeanScore <= 0 || r.MeanScore > 100 {
		t.Errorf("MeanScore = %.2f, want 0 < score <= 100", r.MeanScore)
	}
	if r.Grade == "" {
		t.Errorf("Grade is empty")
	}
	if len(r.WorstSymbols) == 0 || r.WorstSymbols[0].ID != "f.go::hot" {
		t.Errorf("expected hot to be the worst symbol, got %+v", r.WorstSymbols)
	}
}

func TestComputeAuditReport_GradeCountsSumToSymbolCount(t *testing.T) {
	g := graph.New()
	for i := range 20 {
		g.AddNode(&graph.Node{ID: "f.go::F" + strconv.Itoa(i), Kind: graph.KindFunction})
	}
	r := ComputeAuditReport(g, g.AllNodes())
	sum := 0
	for _, n := range r.GradeCounts {
		sum += n
	}
	if sum != r.SymbolCount {
		t.Errorf("grade counts sum to %d, want SymbolCount %d", sum, r.SymbolCount)
	}
}

func TestComputeAuditReport_NonCallableKindsSkipped(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "f.go::T", Name: "T", Kind: graph.KindType})
	g.AddNode(&graph.Node{ID: "f.go::V", Name: "V", Kind: graph.KindVariable})
	g.AddNode(&graph.Node{ID: "f.go::F", Name: "F", Kind: graph.KindFunction})
	r := ComputeAuditReport(g, g.AllNodes())
	if r.SymbolCount != 1 {
		t.Errorf("SymbolCount = %d, want 1 (only function counted)", r.SymbolCount)
	}
}
