package resolver

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// #559: outside extension methods, C# overload sets bound to whichever
// declaration came first — arity evidence (edge arg_count vs candidate
// param_count) was consulted nowhere else. These pins drive the calls
// through the real extractor + resolver, so they cover both halves: the
// receiverless arg_count stamp and the resolver-side narrowing.

// csharpBoundCallTo finds the single resolved call edge from `from` to a
// node named `name`. Overload node IDs carry a line disambiguator
// (Write_L4), so matching goes through the resolved node's Name; an edge
// still parked at unresolved:: has no node and correctly fails the pin.
func csharpBoundCallTo(t *testing.T, g graph.Store, from, name string) *graph.Node {
	t.Helper()
	var found *graph.Node
	for _, e := range g.GetOutEdges(from) {
		if e.Kind != graph.EdgeCalls {
			continue
		}
		if n := g.GetNode(e.To); n != nil && n.Name == name {
			require.Nil(t, found, "expected exactly one resolved call from %s to %s", from, name)
			found = n
		}
	}
	require.NotNil(t, found, "no resolved call edge from %s to %s", from, name)
	return found
}

func TestCSharpOverloadArity_TypedReceiverPicksMatchingOverload(t *testing.T) {
	// zzet's measured repro: the 1-param overload declared FIRST, the
	// call supplies three arguments. Declaration order used to win.
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib/Writer.cs": `namespace App.Lib {
    public class Writer {
        public void Write(string s) { }
        public void Write(string s, int n, bool flush) { }
    }
}`,
		"Svc/Report.cs": `namespace App.Svc {
    public class Report {
        public void Run() {
            Writer w = new Writer();
            w.Write("a", 1, true);
        }
    }
}`,
	})
	New(g).ResolveAll()

	to := csharpBoundCallTo(t, g, "Svc/Report.cs::Report.Run", "Write")
	pc, _ := to.Meta["param_count"].(int)
	require.Equal(t, 3, pc, "arg_count=3 must bind the 3-param overload, got %s", to.ID)
}

func TestCSharpOverloadArity_TypedReceiverOtherDeclarationOrder(t *testing.T) {
	// The mirror: 3-param declared first, call supplies ONE argument.
	// Together with the test above this proves the pick is arity, not
	// declaration order in either direction.
	g := buildCSharpResolverGraph(t, map[string]string{
		"Lib/Writer.cs": `namespace App.Lib {
    public class Writer {
        public void Write(string s, int n, bool flush) { }
        public void Write(string s) { }
    }
}`,
		"Svc/Report.cs": `namespace App.Svc {
    public class Report {
        public void Run() {
            Writer w = new Writer();
            w.Write("a");
        }
    }
}`,
	})
	New(g).ResolveAll()

	to := csharpBoundCallTo(t, g, "Svc/Report.cs::Report.Run", "Write")
	pc, _ := to.Meta["param_count"].(int)
	require.Equal(t, 1, pc, "arg_count=1 must bind the 1-param overload, got %s", to.ID)
}

func TestCSharpOverloadArity_ReceiverlessSameFilePick(t *testing.T) {
	// Receiverless calls carried no arg_count at all (the stamp was
	// scoped to member calls), so the same-file tier took the first
	// same-file overload.
	g := buildCSharpResolverGraph(t, map[string]string{
		"Svc/Emitter.cs": `namespace App.Svc {
    public class Emitter {
        public void Emit(string s) { }
        public void Emit(string s, int n) { }
        public void Run() {
            Emit("x", 1);
        }
    }
}`,
	})
	New(g).ResolveAll()

	to := csharpBoundCallTo(t, g, "Svc/Emitter.cs::Emitter.Run", "Emit")
	pc, _ := to.Meta["param_count"].(int)
	require.Equal(t, 2, pc, "receiverless arg_count=2 must bind the 2-param overload, got %s", to.ID)
}

func TestCSharpOverloadArity_NamedArgumentsCount(t *testing.T) {
	// Named arguments are `argument` nodes like positional ones — three
	// named args must pick the 3-param overload declared second. Also a
	// named arg that skips an optional (window [1,3]) must keep binding.
	g := buildCSharpResolverGraph(t, map[string]string{
		"Svc/Gauge.cs": `namespace App.Svc {
    public class Gauge {
        public void Tune(string tag) { }
        public void Tune(string tag, int level, bool loud) { }
        public void Adjust(string tag, int scale = 1, bool loud = false) { }
        public void Run() {
            Gauge g = new Gauge();
            g.Tune(tag: "a", level: 2, loud: true);
            g.Adjust("a", loud: true);
        }
    }
}`,
	})
	New(g).ResolveAll()

	tune := csharpBoundCallTo(t, g, "Svc/Gauge.cs::Gauge.Run", "Tune")
	pc, _ := tune.Meta["param_count"].(int)
	require.Equal(t, 3, pc, "three named args must bind the 3-param overload, got %s", tune.ID)

	adjust := csharpBoundCallTo(t, g, "Svc/Gauge.cs::Gauge.Run", "Adjust")
	require.Equal(t, "Adjust", adjust.Name, "optional-skipping named call must stay bound")
}

func TestCSharpOverloadArity_GenericOverloadByTypeArgs(t *testing.T) {
	// Explicit type arguments split a generic/non-generic ordinary pair:
	// Pack<int>(5) must bind the generic overload declared second.
	g := buildCSharpResolverGraph(t, map[string]string{
		"Svc/Boxer.cs": `namespace App.Svc {
    public class Boxer {
        public void Pack(string s) { }
        public void Pack<T>(T item) { }
        public void Run() {
            Boxer b = new Boxer();
            b.Pack<int>(5);
        }
    }
}`,
	})
	New(g).ResolveAll()

	to := csharpBoundCallTo(t, g, "Svc/Boxer.cs::Boxer.Run", "Pack")
	tp, _ := to.Meta["method_type_params"].(string)
	require.NotEmpty(t, tp, "explicit <int> must bind the generic overload, got %s", to.ID)
}

func TestCSharpOverloadArity_OutVarArgumentCounts(t *testing.T) {
	// `out var n` is one argument — the 2-arg receiverless call must
	// bind the out-parameter overload declared second.
	g := buildCSharpResolverGraph(t, map[string]string{
		"Svc/Splitter.cs": `namespace App.Svc {
    public class Splitter {
        public bool TryCut(string s) { return false; }
        public bool TryCut(string s, out int n) { n = 0; return true; }
        public void Run() {
            TryCut("a", out var n);
        }
    }
}`,
	})
	New(g).ResolveAll()

	to := csharpBoundCallTo(t, g, "Svc/Splitter.cs::Splitter.Run", "TryCut")
	pc, _ := to.Meta["param_count"].(int)
	require.Equal(t, 2, pc, "out var counts as an argument, got %s", to.ID)
}

func TestCSharpMethodAcceptsArgCount_ZeroParamIsRealArity(t *testing.T) {
	// For an ordinary method, param_count == 0 is a genuine arity —
	// unlike an extension method, which always declares its `this`
	// parameter (the case csharpExtensionAcceptsArgCount special-cases).
	zero := &graph.Node{ID: "x.cs::T.Ping", Meta: map[string]any{"param_count": 0}}
	unstamped := &graph.Node{ID: "x.cs::T.Old", Meta: map[string]any{}}

	call0 := &graph.Edge{Meta: map[string]any{"arg_count": 0}}
	require.True(t, csharpMethodAcceptsArgCount(call0, zero), "0 args fit a 0-param method")

	call1 := &graph.Edge{Meta: map[string]any{"arg_count": 1}}
	require.False(t, csharpMethodAcceptsArgCount(call1, zero), "1 arg cannot fit a 0-param method")
	require.True(t, csharpMethodAcceptsArgCount(call1, unstamped), "no stamp = no evidence, accept")
}

func TestCSharpNarrowMethodByApplicability_OptionalAndVariadicWindow(t *testing.T) {
	// Ordinary methods use the same [required, declared] window as
	// extensions, minus the `this`-slot discount: optional parameters
	// lower the floor, a params array removes the ceiling.
	optional := &graph.Node{ID: "x.cs::T.Opt", Meta: map[string]any{
		"param_count": 3, "param_required": 1,
	}}
	variadic := &graph.Node{ID: "x.cs::T.Var", Meta: map[string]any{
		"param_count": 2, "param_required": 1, "param_variadic": true,
	}}

	call2 := &graph.Edge{Meta: map[string]any{"arg_count": 2}}
	require.True(t, csharpMethodAcceptsArgCount(call2, optional), "2 args inside [1,3]")

	call4 := &graph.Edge{Meta: map[string]any{"arg_count": 4}}
	require.False(t, csharpMethodAcceptsArgCount(call4, optional), "4 args above the declared count")
	require.True(t, csharpMethodAcceptsArgCount(call4, variadic), "params tail accepts any count above required")

	// A filter that would empty the set keeps it — narrowing must never
	// turn a bind into a refusal.
	call9 := &graph.Edge{Meta: map[string]any{"arg_count": 9}}
	got := csharpNarrowMethodByApplicability(call9, []*graph.Node{optional})
	require.Equal(t, []*graph.Node{optional}, got)
}
