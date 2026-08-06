package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// accessEdge returns the first EdgeReads/EdgeWrites out of fromID whose
// target (resolved or unresolved) names member.
func accessEdge(g graph.Store, fromID, member string) *graph.Edge {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind != graph.EdgeReads && e.Kind != graph.EdgeWrites {
			continue
		}
		if graph.IsUnresolvedTarget(e.To) {
			if name := graph.UnresolvedName(e.To); name == "*."+member || name == member {
				return e
			}
			continue
		}
		if n := g.GetNode(e.To); n != nil && n.Name == member {
			return e
		}
	}
	return nil
}

// A typed C# property read must bind the receiver type's own property —
// not a same-named property on an unrelated class — through the existing
// resolveFieldRef cascade.
func TestResolveCSharp_MemberAccessBindsReceiverField(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib.cs": `namespace App {
    public class StencilDecoy { public string Title { get; set; } }
    public class Plaque { public string Title { get; set; } }
    public class Reader {
        public string Get() { Plaque p = new Plaque(); return p.Title; }
        public void Set() { Plaque p = new Plaque(); p.Title = "x"; }
    }
}`,
	})
	New(g).ResolveAll()

	read := accessEdge(g, "Lib.cs::Reader.Get", "Title")
	require.NotNil(t, read, "p.Title must produce a read edge")
	assert.True(t, strings.Contains(read.To, "Plaque.Title"),
		"typed receiver binds the receiver type's property, got %q", read.To)
	assert.False(t, strings.Contains(read.To, "StencilDecoy"),
		"decoy must not win, got %q", read.To)
	assert.GreaterOrEqual(t, read.Confidence, 0.9,
		"exact receiver-type field match binds confidently")

	write := accessEdge(g, "Lib.cs::Reader.Set", "Title")
	require.NotNil(t, write, "p.Title = ... must produce a write edge")
	assert.Equal(t, graph.EdgeWrites, write.Kind)
	assert.True(t, strings.Contains(write.To, "Plaque.Title"),
		"write binds the same way, got %q", write.To)
}
