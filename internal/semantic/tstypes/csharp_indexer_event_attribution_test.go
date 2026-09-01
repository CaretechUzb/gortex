package tstypes

import (
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// Issue #728. An indexer or an accessor-bearing event holds executable
// code but used to own no node, so the extractor's line fallback parked
// its body call on whatever member covered the physical line - a
// same-line property - and caller adoption promoted that stub to a
// confident resolved edge. The property in these fixtures is
// `public int Slot => 1`: its entire body is a literal, so any call edge
// leaving it is false by construction.
//
// The trigger is specifically a FIELD-KIND neighbour. Pair the indexer
// with a method instead and line containment finds the method either
// way, which is why a method-neighbour fixture shows nothing.
//
// Both halves are asserted per shape: the call lands on the member that
// authored it, and the neighbour carries nothing.
func TestCSharp_IndexerAndEventBodyCallsResolveFromTheirOwnMember(t *testing.T) {
	for _, tc := range []struct {
		name, member, body string
	}{
		{
			name:   "indexer beside property",
			member: "this[]",
			body:   "public int Slot => 1; public int this[int i] { get { worker.Run(); return 1; } }",
		},
		{
			name:   "event accessor beside property",
			member: "Ev",
			body:   "public int Slot => 1; public event System.EventHandler Ev { add { worker.Run(); } remove { } }",
		},
		{
			name:   "indexer alone on its line",
			member: "this[]",
			body:   "public int this[int i] { get { worker.Run(); return 1; } }",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, dir := buildFixture(t, map[string]string{
				"A/Svc.cs": csSvc,
				"B/App.cs": `namespace B {
    public class App {
        private Svc worker;
        ` + tc.body + `
    }
}
`,
			})
			p := NewProvider(CSharpSpec(), zap.NewNop())
			if _, err := p.Enrich(g, dir); err != nil {
				t.Fatal(err)
			}
			run := nodeByNameKind(t, g, "Run", graph.KindMethod)
			member := nodeByNameKind(t, g, tc.member, graph.KindField)
			if e := callEdgeTo(g, member.ID, run.ID); e == nil {
				t.Fatalf("body call not resolved from %s; edges: %v", tc.member, g.GetOutEdges(member.ID))
			} else {
				assertASTProvenance(t, e, "csharp-types")
			}
			for _, e := range g.AllEdges() {
				if e == nil || e.Kind != graph.EdgeCalls {
					continue
				}
				if e.From != member.ID {
					t.Errorf("call edge from %s: a member that authored no call must carry none (to %s, conf %.2f)",
						e.From, e.To, e.Confidence)
				}
			}
		})
	}
}
