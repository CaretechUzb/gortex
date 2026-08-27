package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// A tests edge is DERIVED: the test-linkage pass clones a test caller's
// calls edges, meta-free. Routing such a clone through call resolution
// re-runs the bind WITHOUT the original's receiver evidence, bypassing
// every receiver-gated guard. Field shape: a `List<int>` receiver whose
// calls edge the extension shape guard correctly refuses (`this
// List<string>` conflicts) - the naked tests clone of the same site was
// bound by the untyped pool-unique fallback at 0.75. The resolver must
// never bind a tests edge; the tests layer follows its calls edge.
func TestResolveAll_NeverBindsATestsEdge(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"SpecGenArgs.cs": `using System.Collections.Generic;

namespace Probe.Spec.GenArgs {
    public static class ListStretch {
        public static int Total(this List<string> xs, int pad) { return pad; }
    }

    public class GenRunner {
        public int Run() {
            var xs = new List<int>();
            return xs.Total(3);
        }
    }
}`,
	})

	callerID := "SpecGenArgs.cs::GenRunner.Run"
	// The clone the test-linkage pass would have minted while the call
	// was unresolved: same site, no receiver meta.
	g.AddEdge(&graph.Edge{
		From: callerID, To: "unresolved::*.Total", Kind: graph.EdgeTests,
		FilePath: "SpecGenArgs.cs", Line: 11, Origin: graph.OriginASTInferred,
	})

	New(g).ResolveAll()

	var testsEdge *graph.Edge
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeTests {
			testsEdge = e
		}
	}
	require.NotNil(t, testsEdge, "fixture: the tests clone must survive resolution")
	assert.True(t, graph.IsUnresolvedTarget(testsEdge.To),
		"the resolver bound a derived tests edge (to %s) - the naked clone bypasses the receiver-gated guards", testsEdge.To)

	// Control: the CALLS edge at the same site keeps its own verdict -
	// the shape guard refuses the List<int> vs `this List<string>`
	// conflict, so it stays honestly unresolved too, WITH its receiver
	// evidence intact.
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeCalls && e.Line == 11 {
			assert.True(t, graph.IsUnresolvedTarget(e.To),
				"control drifted: the guarded calls edge bound to %s", e.To)
		}
	}
}
