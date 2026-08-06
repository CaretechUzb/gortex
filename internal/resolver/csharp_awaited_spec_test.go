package resolver

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// fooCallEdge returns the Foo call edge out of fromID — the resolved edge
// when binding ran, the unresolved stub otherwise — so specs can assert
// both the target and the confidence it was bound at.
func fooCallEdge(g graph.Store, fromID string) *graph.Edge {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if graph.IsUnresolvedTarget(e.To) {
			if name := graph.UnresolvedName(e.To); name == "*.Foo" || name == "Foo" {
				return e
			}
			continue
		}
		if n := g.GetNode(e.To); n != nil && n.Name == "Foo" {
			return e
		}
	}
	return nil
}

// Awaited receivers evaluate to the T inside Task<T>. With a same-named
// decoy member in scope, both call shapes must bind the awaited type's
// member confidently — not luck-pick between candidates at zero confidence.
func TestResolveCSharp_AwaitedReceivers(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib.cs": `using System.Threading.Tasks;
namespace App {
    public class DrumScale { public int Foo(int id) { return id; } }
    public class NoiseMaker { public int Foo(int id) { return 0; } }
    public class ScaleLoader {
        public Task<DrumScale> LoadAsync(int id) { return Task.FromResult(new DrumScale()); }
        public async Task<int> WeighLoaded(int id) {
            var scale = await LoadAsync(id);
            return scale.Foo(id);
        }
        public async Task<int> WeighInline(int id) {
            return (await LoadAsync(id)).Foo(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	local := fooCallEdge(g, "Lib.cs::ScaleLoader.WeighLoaded")
	require.NotNil(t, local, "awaited-local Foo() must produce a call edge")
	assert.True(t, strings.Contains(local.To, "DrumScale.Foo"),
		"awaited local binds the unwrapped type's member, got %q", local.To)
	assert.GreaterOrEqual(t, local.Confidence, 0.9,
		"typed receiver must bind confidently, not by fallback luck")

	inline := fooCallEdge(g, "Lib.cs::ScaleLoader.WeighInline")
	require.NotNil(t, inline, "(await ...).Foo() must produce a call edge")
	assert.True(t, strings.Contains(inline.To, "DrumScale.Foo"),
		"inline awaited receiver binds the unwrapped type's member, got %q", inline.To)
	assert.GreaterOrEqual(t, inline.Confidence, 0.9,
		"typed receiver must bind confidently, not by fallback luck")
}
