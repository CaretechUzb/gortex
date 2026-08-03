package resolver

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// R5 (PR #432 review): the global-using index was rebuilt by a
// workspace-wide KindFile scan on EVERY scoped resolve — the cache was
// cleared at each entry, so a one-line save of any C# file re-paid
// O(all files). The index must persist across scoped resolves and
// reconcile only the files each resolve names.

// kindFileScanCounter counts full KindFile iterations — the signature
// of the global-using index rebuild.
type kindFileScanCounter struct {
	graph.Store
	kindFileScans int
}

func (s *kindFileScanCounter) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	if kind == graph.KindFile {
		s.kindFileScans++
	}
	return s.Store.NodesByKind(kind)
}

func csharpVisCacheFixture(t *testing.T) graph.Store {
	t.Helper()
	return buildCSharpResolverGraph(t, map[string]string{
		"W.cs": `namespace W {
    public class Crate {}
}`,
		"E1.cs": `using W;
namespace LibA {
    public static class E1 { public static void Foo(this Crate c) {} }
}`,
		"E2.cs": `using W;
namespace LibB {
    public static class E2 { public static void Foo(this Crate c) {} }
}`,
		// Globals exist (so the index has content) but import neither
		// candidate namespace: the call below stays ambiguous, so every
		// scoped resolve re-enters the visibility lookup.
		"Usings.cs": `global using LibX;
`,
		"Caller.cs": `using W;
namespace App {
    public class Runner {
        public void Run() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}`,
	})
}

// TestResolveCSharp_GlobalUsingIndexPersistsAcrossScopedResolves: the
// first scoped resolve seeds the index (one workspace scan); later
// scoped resolves must reuse the seeded index instead of dropping and
// rebuilding it. The canary key pins the mechanism: a drop-and-rescan
// loses it, a persistent index keeps it. (A raw scan count cannot
// isolate this — unrelated per-pass caches also walk KindFile.)
func TestResolveCSharp_GlobalUsingIndexPersistsAcrossScopedResolves(t *testing.T) {
	counter := &kindFileScanCounter{Store: csharpVisCacheFixture(t)}
	r := New(counter)

	r.ResolveFileAndIncoming("Caller.cs")
	require.Greater(t, counter.kindFileScans, 0,
		"the first scoped resolve enters the visibility lookup and seeds the index")

	target := fooCallTarget(counter, "Caller.cs::Runner.Run")
	require.True(t, graph.IsUnresolvedTarget(target),
		"fixture: the two-way tie must stay unresolved so later resolves re-enter the lookup, got %q", target)

	r.csharpNSMu.Lock()
	require.NotNil(t, r.csharpGlobalByDir, "the lookup must have seeded the index")
	r.csharpGlobalByDir["__canary__"] = []string{"x"}
	r.csharpNSMu.Unlock()

	for i := 0; i < 3; i++ {
		r.ResolveFileAndIncoming("Caller.cs")
	}

	r.csharpNSMu.Lock()
	_, canary := r.csharpGlobalByDir["__canary__"]
	r.csharpNSMu.Unlock()
	assert.True(t, canary,
		"nothing changed between resolves — the global-using index must persist instead of being rebuilt by a workspace scan at every entry")
}

// TestResolveCSharp_GlobalUsingIndexReconcilesEditedFile: persistence
// must not mean staleness — when a resolve names a file whose
// global-using stamp changed, the index reconciles that file and the
// new visibility decides the verdict.
func TestResolveCSharp_GlobalUsingIndexReconcilesEditedFile(t *testing.T) {
	store := csharpVisCacheFixture(t)
	r := New(store)
	r.ResolveFileAndIncoming("Caller.cs")
	require.True(t, graph.IsUnresolvedTarget(fooCallTarget(store, "Caller.cs::Runner.Run")),
		"baseline: LibX grants nothing — the tie stays unresolved")

	// Simulate a save of Usings.cs: the file node's stamp now imports
	// LibA. A scoped resolve NAMING Usings.cs must fold the change in.
	usings := store.GetFileNodes("Usings.cs")
	require.NotEmpty(t, usings)
	for _, n := range usings {
		if n.Kind == graph.KindFile {
			n.Meta["usings"] = []string{"LibA"}
			n.Meta["global_usings"] = []string{"LibA"}
			n.Meta["scoped_usings"] = []string{"|LibA"}
		}
	}
	r.ResolveFileAndIncoming("Usings.cs")
	r.ResolveFileAndIncoming("Caller.cs")
	target := fooCallTarget(store, "Caller.cs::Runner.Run")
	assert.Contains(t, target, "E1.Foo",
		"the reconciled global using makes exactly LibA visible — the call must bind")
}

// TestResolveCSharp_GlobalUsingStaticPropagates: `global using static
// LibA.E1;` is compilation-scoped — E1's extension methods are in scope
// for EVERY file of the unit, not just the declaring one.
func TestResolveCSharp_GlobalUsingStaticPropagates(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"W.cs": `namespace W {
    public class Crate {}
}`,
		"E1.cs": `using W;
namespace LibA {
    public static class E1 { public static int Foo(this Crate c) { return 1; } }
}`,
		"Usings.cs": `global using static LibA.E1;
`,
		"Caller.cs": `using W;
namespace App {
    public class Runner {
        public void Run() {
            Crate c = new Crate();
            c.Foo();
        }
    }
}`,
	})
	New(g).ResolveAll()

	target := fooCallTarget(g, "Caller.cs::Runner.Run")
	require.NotEmpty(t, target)
	assert.Contains(t, target, "E1.Foo",
		"the global using static puts E1's extensions in scope for the whole unit, got %q", target)
}
