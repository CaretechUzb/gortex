package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestAppendUniqueFrameworkEdgesUsesExactIdentity(t *testing.T) {
	zero := &graph.Edge{}
	first := &graph.Edge{
		From: "caller", To: "callee", Kind: graph.EdgeCalls,
		FilePath: "x.go", Line: 7, Origin: graph.OriginASTResolved,
		Meta: map[string]any{"version": "first"},
	}
	duplicatePayload := &graph.Edge{
		From: first.From, To: first.To, Kind: first.Kind,
		FilePath: first.FilePath, Line: first.Line,
		Origin: graph.OriginASTInferred, Confidence: 0.9,
		Meta: map[string]any{"version": "second"},
	}
	left := &graph.Edge{From: "a", To: "b\x00c", Kind: graph.EdgeCalls}
	right := &graph.Edge{From: "a\x00b", To: "c", Kind: graph.EdgeCalls}

	seen := make(map[graph.EdgeIdentity]struct{})
	got := appendUniqueFrameworkEdges(nil, seen, nil, zero, first, duplicatePayload, left, right)
	if len(got) != 4 || len(seen) != 4 {
		t.Fatalf("unique edges = rows:%d seen:%d, want 4/4", len(got), len(seen))
	}
	if got[0] != zero {
		t.Fatal("real zero-valued edge was confused with nil")
	}
	if got[1] != first {
		t.Fatal("duplicate identity did not preserve the first edge")
	}
	if got[2] != left || got[3] != right {
		t.Fatalf("NUL-bearing identities collided or reordered: %#v", got[2:])
	}
}
