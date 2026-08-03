package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestOverrideDispatchCandidatesExactRefetchDropsStaleRows(t *testing.T) {
	g := graph.New()
	const repo = "repo"
	typeA := &graph.Node{
		ID: repo + "::A", Kind: graph.KindType, Name: "A", Language: "java", RepoPrefix: repo,
		Meta: map[string]any{"scope_parent": "Base"},
	}
	typeB := &graph.Node{
		ID: repo + "::B", Kind: graph.KindType, Name: "B", Language: "java", RepoPrefix: repo,
		Meta: map[string]any{"scope_parent": "Base"},
	}
	methodA := &graph.Node{
		ID: repo + "::A.run", Kind: graph.KindMethod, Name: "run", Language: "java", RepoPrefix: repo,
		Meta: map[string]any{"receiver": "A"},
	}
	methodB := &graph.Node{
		ID: repo + "::B.run", Kind: graph.KindMethod, Name: "run", Language: "java", RepoPrefix: repo,
		Meta: map[string]any{"receiver": "B"},
	}
	caller := &graph.Node{
		ID: repo + "::Caller.call", Kind: graph.KindMethod, Name: "call", Language: "java", RepoPrefix: repo,
	}
	retargeted := &graph.Edge{
		From: caller.ID, To: "unresolved::*.run", Kind: graph.EdgeCalls, FilePath: "Caller.java", Line: 1,
	}
	becameSpeculative := &graph.Edge{
		From: caller.ID, To: "unresolved::*.run", Kind: graph.EdgeCalls, FilePath: "Caller.java", Line: 2,
	}
	g.AddBatch(
		[]*graph.Node{typeA, typeB, methodA, methodB, caller},
		[]*graph.Edge{retargeted, becameSpeculative},
	)

	r := New(g)
	candidates := r.collectOverrideDispatchCandidates()
	if !candidates.exact || len(candidates.java) != 2 {
		t.Fatalf("candidate census = %#v, want two exact Java identities", candidates)
	}

	oldTo := retargeted.To
	retargeted.To = "unresolved::*.other"
	g.ReindexEdges([]graph.EdgeReindex{{Edge: retargeted, OldTo: oldTo}})
	becameSpeculative.Meta = map[string]any{graph.MetaSpeculative: true}
	g.ReindexEdges([]graph.EdgeReindex{{Edge: becameSpeculative, OldTo: becameSpeculative.To}})

	if got := r.resolveJavaOverrideDispatchCandidates(candidates); got != 0 {
		t.Fatalf("stale candidate pass mutated %d edges, want 0", got)
	}
	for _, edge := range g.GetOutEdges(caller.ID) {
		if edge == nil {
			continue
		}
		switch edge.Line {
		case 1:
			if edge.To != "unresolved::*.other" {
				t.Fatalf("retargeted edge changed to %q", edge.To)
			}
		case 2:
			if edge.To != "unresolved::*.run" || !edge.IsSpeculative() {
				t.Fatalf("speculative edge was mutated: %#v", edge)
			}
		}
	}
}
