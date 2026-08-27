package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Generic type arguments gate the interface-dispatch fan-out: a receiver
// declared IBoxStore<Crate> can never dispatch into the class implementing
// IBoxStore<Widget>, so the fan-out must not fabricate that usage. The
// filter is evidence-gated on BOTH sides — the receiver's declared field
// type and the implementor's stamped base-list arguments — and absence of
// either keeps today's full fan-out (precision only ever improves).
func TestResolveCSharpInterfaceDispatch_GenericTypeArgsGateFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        T Fetch(int id);
    }
    public class WidgetBoxStore : IBoxStore<Widget> {
        public Widget Fetch(int id) { return new Widget(); }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public Crate Fetch(int id) { return new Crate(); }
    }
}`,
		"CrateFlow.cs": `namespace App {
    public class CrateFlow {
        private readonly IBoxStore<Crate> _store;
        public CrateFlow(IBoxStore<Crate> store) { _store = store; }
        public Crate Pull(int id) {
            return _store.Fetch(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "CrateFlow.cs::CrateFlow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_store", "Stores.cs::IBoxStore.Fetch")

	ResolveCSharpInterfaceDispatch(g)

	var targets []string
	for _, e := range g.GetOutEdges(callerID) {
		if isIfaceDispatchEdge(e) {
			targets = append(targets, e.To)
		}
	}
	assert.Contains(t, targets, "Stores.cs::CrateBoxStore.Fetch",
		"the type-compatible implementation still receives the fan-out")
	assert.NotContains(t, targets, "Stores.cs::WidgetBoxStore.Fetch",
		"an IBoxStore<Crate> receiver can never dispatch to the IBoxStore<Widget> impl")
}

// bindFieldReceiverCall mirrors what the enrichment/LSP tiers do on a live
// store for a FIELD-receiver member call the core resolver leaves
// unresolved: a resolved call edge lands at the same site, while the
// extraction's own unresolved companion edge (carrying receiver_name)
// stays alongside it. The dispatch pass reads the receiver evidence from
// that companion - exactly the join a production store requires.
func bindFieldReceiverCall(t *testing.T, g graph.Store, callerID, receiver, target string) {
	t.Helper()
	var companion *graph.Edge
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeCalls && graph.IsUnresolvedTarget(e.To) &&
			e.Meta != nil && e.Meta["receiver_name"] == receiver {
			companion = e
			break
		}
	}
	require.NotNil(t, companion, "fixture: the extraction must leave a receiver_name companion edge")
	g.AddEdge(&graph.Edge{
		From: callerID, To: target, Kind: graph.EdgeCalls,
		FilePath: companion.FilePath, Line: companion.Line,
		Origin: graph.OriginASTResolved, Confidence: 0.95,
	})
}

// The sibling mechanism is gated the same way: a call bound directly to the
// Widget implementation's own method must not fan into the Crate
// implementation — they implement different constructed interfaces — while
// the erased interface member itself (argument-less evidence) still
// receives the site.
func TestResolveCSharpInterfaceDispatch_SiblingFanoutRespectsTypeArgs(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        T Fetch(int id);
    }
    public class WidgetBoxStore : IBoxStore<Widget> {
        public Widget Fetch(int id) { return new Widget(); }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public Crate Fetch(int id) { return new Crate(); }
    }
    public class WidgetUser {
        public Widget Load(int id) {
            WidgetBoxStore store = new WidgetBoxStore();
            return store.Fetch(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Stores.cs::WidgetUser.Load"
	require.Contains(t, callTargetsFrom(g, callerID), "Stores.cs::WidgetBoxStore.Fetch",
		"fixture: the typed-local call must bind to the Widget implementation")

	ResolveCSharpInterfaceDispatch(g)

	var targets []string
	for _, e := range g.GetOutEdges(callerID) {
		if isIfaceDispatchEdge(e) {
			targets = append(targets, e.To)
		}
	}
	assert.Contains(t, targets, "Stores.cs::IBoxStore.Fetch",
		"the erased interface member still receives the sibling site")
	assert.NotContains(t, targets, "Stores.cs::CrateBoxStore.Fetch",
		"a Widget-impl site never fans into the Crate impl - different constructed interfaces")
}

// An open-generic implementor (Relay<T> : IBoxStore<T>) carries no stamp and
// can bind ANY argument - it must stay in every fan-out.
func TestResolveCSharpInterfaceDispatch_OpenGenericImplStaysInFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        T Fetch(int id);
    }
    public class Relay<T> : IBoxStore<T> {
        public T Fetch(int id) { return default(T); }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public Crate Fetch(int id) { return new Crate(); }
    }
}`,
		"CrateFlow.cs": `namespace App {
    public class CrateFlow {
        private readonly IBoxStore<Crate> _store;
        public CrateFlow(IBoxStore<Crate> store) { _store = store; }
        public Crate Pull(int id) {
            return _store.Fetch(id);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "CrateFlow.cs::CrateFlow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_store", "Stores.cs::IBoxStore.Fetch")

	ResolveCSharpInterfaceDispatch(g)

	var targets []string
	for _, e := range g.GetOutEdges(callerID) {
		if isIfaceDispatchEdge(e) {
			targets = append(targets, e.To)
		}
	}
	assert.Contains(t, targets, "Stores.cs::Relay.Fetch",
		"an open-generic implementor can bind any argument and must stay in the fan-out")
	assert.Contains(t, targets, "Stores.cs::CrateBoxStore.Fetch")
}

// dispatchTargets returns the fan-out targets minted from callerID.
func dispatchTargets(g graph.Store, callerID string) []string {
	var targets []string
	for _, e := range g.GetOutEdges(callerID) {
		if isIfaceDispatchEdge(e) {
			targets = append(targets, e.To)
		}
	}
	return targets
}

// Review RED 1: a receiver field typed with an ENCLOSING generic type's
// parameter (Outer<T> { class Flow { IBoxStore<T> _store; } }) closes
// nothing - the site must keep the FULL fan-out, and the nested
// implementor's own bogus stamp must not survive either.
func TestResolveCSharpInterfaceDispatch_EnclosingTypeParamNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        T Fetch(int id);
    }
    public class WidgetBoxStore : IBoxStore<Widget> {
        public Widget Fetch(int id) { return new Widget(); }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public Crate Fetch(int id) { return new Crate(); }
    }
}`,
		"Nested.cs": `namespace App {
    public class Outer<T> {
        public class Flow {
            private readonly IBoxStore<T> _store;
            public Flow(IBoxStore<T> store) { _store = store; }
            public T Pull(int id) {
                return _store.Fetch(id);
            }
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Nested.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_store", "Stores.cs::IBoxStore.Fetch")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Stores.cs::CrateBoxStore.Fetch",
		"an open receiver argument filters nothing")
	assert.Contains(t, targets, "Stores.cs::WidgetBoxStore.Fetch",
		"an open receiver argument filters nothing")
}

// Review RED 3: two same-named member calls on one line share a single
// unresolved companion edge - the receiver evidence is ambiguous, so the
// site must keep the FULL fan-out rather than applying one receiver's
// arguments to both calls.
func TestResolveCSharpInterfaceDispatch_AmbiguousSameLineReceiverNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        int Fetch(int id);
    }
    public class WidgetBoxStore : IBoxStore<Widget> {
        public int Fetch(int id) { return 1; }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public int Fetch(int id) { return 2; }
    }
    public class Flow {
        private readonly IBoxStore<Crate> _crates;
        private readonly IBoxStore<Widget> _widgets;
        public Flow(IBoxStore<Crate> c, IBoxStore<Widget> w) { _crates = c; _widgets = w; }
        public int Pull() { return _crates.Fetch(_widgets.Fetch(1)); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Stores.cs::Flow.Pull"
	// One bound edge stands in for both same-line calls (same from/to/line).
	var companion *graph.Edge
	for _, e := range g.GetOutEdges(callerID) {
		if e != nil && e.Kind == graph.EdgeCalls && graph.IsUnresolvedTarget(e.To) {
			companion = e
			break
		}
	}
	require.NotNil(t, companion)
	g.AddEdge(&graph.Edge{
		From: callerID, To: "Stores.cs::IBoxStore.Fetch", Kind: graph.EdgeCalls,
		FilePath: companion.FilePath, Line: companion.Line,
		Origin: graph.OriginASTResolved, Confidence: 0.95,
	})

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Stores.cs::CrateBoxStore.Fetch",
		"ambiguous receiver evidence filters nothing")
	assert.Contains(t, targets, "Stores.cs::WidgetBoxStore.Fetch",
		"ambiguous receiver evidence filters nothing")
}

// Multi-argument closures compare as one normalized list end-to-end - the
// string-equality contract between extractor stamp and receiver stamp is
// exactly what would regress silently without a pin.
func TestResolveCSharpInterfaceDispatch_MultiArgClosureGatesFanout(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Pairs.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IPair<K, V> {
        int Fetch(int id);
    }
    public class WidgetPair : IPair<int, Widget> {
        public int Fetch(int id) { return 1; }
    }
    public class CratePair : IPair<int, Crate> {
        public int Fetch(int id) { return 2; }
    }
    public class Flow {
        private readonly IPair<int, Crate> _pairs;
        public Flow(IPair<int, Crate> p) { _pairs = p; }
        public int Pull() { return _pairs.Fetch(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Pairs.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_pairs", "Pairs.cs::IPair.Fetch")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Pairs.cs::CratePair.Fetch")
	assert.NotContains(t, targets, "Pairs.cs::WidgetPair.Fetch",
		"an IPair<int,Crate> receiver never dispatches to the IPair<int,Widget> impl")
}

// Review RED (revision 1): two DIFFERENT member calls on one line, each on
// its own field of the same generic interface closed with different
// arguments — `_crates.Fetch(1) + _widgets.Save(2)`. The receiver lookup is
// cached per call site, and a cache key without the member identity lets the
// first receiver's arguments poison the second call's gate: WidgetBoxStore.Save
// silently loses its usage. Each member call must gate on ITS OWN receiver.
func TestResolveCSharpInterfaceDispatch_SameLineDistinctMembersKeepOwnReceivers(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Stores.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBoxStore<T> {
        int Fetch(int id);
        int Save(int id);
    }
    public class WidgetBoxStore : IBoxStore<Widget> {
        public int Fetch(int id) { return 1; }
        public int Save(int id) { return 1; }
    }
    public class CrateBoxStore : IBoxStore<Crate> {
        public int Fetch(int id) { return 2; }
        public int Save(int id) { return 2; }
    }
    public class Flow {
        private readonly IBoxStore<Crate> _crates;
        private readonly IBoxStore<Widget> _widgets;
        public Flow(IBoxStore<Crate> c, IBoxStore<Widget> w) { _crates = c; _widgets = w; }
        public int Pull() { return _crates.Fetch(1) + _widgets.Save(2); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Stores.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_crates", "Stores.cs::IBoxStore.Fetch")
	bindFieldReceiverCall(t, g, callerID, "_widgets", "Stores.cs::IBoxStore.Save")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Stores.cs::CrateBoxStore.Fetch",
		"the Fetch call gates on _crates and keeps the Crate impl")
	assert.NotContains(t, targets, "Stores.cs::WidgetBoxStore.Fetch",
		"the Fetch call's receiver is IBoxStore<Crate>")
	assert.Contains(t, targets, "Stores.cs::WidgetBoxStore.Save",
		"the Save call gates on _widgets - the Fetch lookup must not poison it")
	assert.NotContains(t, targets, "Stores.cs::CrateBoxStore.Save",
		"the Save call's receiver is IBoxStore<Widget>")
}

// Review RED (revision 2a): a method parameter shadows the receiver field —
// `IBox<Crate> _box` field, `IBox<Widget> _box` parameter. The receiver
// identifier binds to the PARAMETER, but a bare text lookup finds the field
// and its Crate arguments suppress WidgetBox.Get. Without binding evidence
// the receiver is unknown and the site must keep the full fan-out.
func TestResolveCSharpInterfaceDispatch_ParameterShadowedReceiverNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Boxes.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 1; }
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
        public int Pull(IBox<Widget> _box) {
            return _box.Get(7);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Boxes.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_box", "Boxes.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Boxes.cs::WidgetBox.Get",
		"the receiver is the shadowing IBox<Widget> parameter - its impl must stay in")
	assert.Contains(t, targets, "Boxes.cs::CrateBox.Get",
		"an unknown receiver filters nothing")
}

// Review RED (revision 2b): a `var` local shadows the receiver field the
// same way - the identifier binds to the local, not the field the text
// lookup finds. (An interface-TYPED local never reaches the receiver
// lookup: the tenv binds its call directly and leaves no companion.)
func TestResolveCSharpInterfaceDispatch_LocalShadowedReceiverNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Boxes.cs": `namespace App {
    public class Widget { }
    public class Crate { }
    public interface IBox<T> {
        int Get(int id);
    }
    public class WidgetBox : IBox<Widget> {
        public int Get(int id) { return 1; }
    }
    public class CrateBox : IBox<Crate> {
        public int Get(int id) { return 2; }
    }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
        public int Pull(IBox<Widget> source) {
            var _box = source;
            return _box.Get(7);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Boxes.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_box", "Boxes.cs::IBox.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Boxes.cs::WidgetBox.Get",
		"the receiver is the shadowing IBox<Widget> local - its impl must stay in")
	assert.Contains(t, targets, "Boxes.cs::CrateBox.Get",
		"an unknown receiver filters nothing")
}

// Review RED (revision 3a): a covariant interface (`ISource<out T>`) makes
// ISource<Dog> assignable to an ISource<Animal> receiver, so DogSource.Get
// is a real dispatch target at the site - the closed-and-unequal equality
// gate only models INVARIANT parameters and must stand down entirely when
// the interface declares any variance.
func TestResolveCSharpInterfaceDispatch_CovariantInterfaceNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Sources.cs": `namespace App {
    public class Animal { }
    public class Dog : Animal { }
    public class Cat : Animal { }
    public interface ISource<out T> {
        T Get();
    }
    public class DogSource : ISource<Dog> {
        public Dog Get() { return new Dog(); }
    }
    public class CatSource : ISource<Cat> {
        public Cat Get() { return new Cat(); }
    }
    public class Flow {
        private readonly ISource<Animal> _source;
        public Flow(ISource<Animal> s) { _source = s; }
        public Animal Pull() {
            return _source.Get();
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Sources.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_source", "Sources.cs::ISource.Get")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Sources.cs::DogSource.Get",
		"out T: ISource<Dog> satisfies an ISource<Animal> receiver - the impl must stay in")
	assert.Contains(t, targets, "Sources.cs::CatSource.Get",
		"out T: ISource<Cat> satisfies an ISource<Animal> receiver - the impl must stay in")
}

// Review RED (revision 3b): the contravariant twin (`ISink<in T>`) -
// ISink<Animal> is assignable to an ISink<Dog> receiver.
func TestResolveCSharpInterfaceDispatch_ContravariantInterfaceNeverFilters(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Sinks.cs": `namespace App {
    public class Animal { }
    public class Dog : Animal { }
    public interface ISink<in T> {
        int Put(int item);
    }
    public class AnimalSink : ISink<Animal> {
        public int Put(int item) { return 1; }
    }
    public class DogSink : ISink<Dog> {
        public int Put(int item) { return 2; }
    }
    public class Flow {
        private readonly ISink<Dog> _sink;
        public Flow(ISink<Dog> s) { _sink = s; }
        public int Push() {
            return _sink.Put(3);
        }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Sinks.cs::Flow.Push"
	bindFieldReceiverCall(t, g, callerID, "_sink", "Sinks.cs::ISink.Put")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Sinks.cs::AnimalSink.Put",
		"in T: ISink<Animal> satisfies an ISink<Dog> receiver - the impl must stay in")
	assert.Contains(t, targets, "Sinks.cs::DogSink.Put")
}

// Codex review RED: `IBoxStore<System.Int32>` and `IBoxStore<int>` are the
// SAME constructed interface in different spellings — the gate must fold
// both to one canonical form and RETAIN the edge, never suppress it on a
// spelling mismatch.
func TestResolveCSharpInterfaceDispatch_BCLAliasSpellingsRetainTheEdge(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ints.cs": `namespace App {
    public class Crate { }
    public interface IBoxStore<T> {
        int Fetch(int id);
    }
    public class IntBox : IBoxStore<System.Int32> {
        public int Fetch(int id) { return 1; }
    }
    public class CrateBox : IBoxStore<Crate> {
        public int Fetch(int id) { return 2; }
    }
    public class Flow {
        private readonly IBoxStore<int> _ints;
        public Flow(IBoxStore<int> i) { _ints = i; }
        public int Pull() { return _ints.Fetch(1); }
    }
}`,
	})
	New(g).ResolveAll()

	callerID := "Ints.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_ints", "Ints.cs::IBoxStore.Fetch")

	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	assert.Contains(t, targets, "Ints.cs::IntBox.Fetch",
		"System.Int32 and int spell the same constructed interface - the edge stays")
	assert.NotContains(t, targets, "Ints.cs::CrateBox.Fetch",
		"the genuinely different closure still filters")
}
