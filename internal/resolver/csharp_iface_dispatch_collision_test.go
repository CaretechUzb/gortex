package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// C# type node IDs are filePath + "::" + name: no namespace, no generic
// arity. Two declarations that differ only in those dimensions collide,
// and emitContainer drops the second. That was a harmless
// over-approximation while the dispatch fan-out was unfiltered - the two
// declarations merged and every target stayed. Once a gate reads
// evidence off the surviving node, the loser's evidence is gone and the
// gate filters on the winner's.
//
// This file covers that axis: every way two declarations can land on one
// ID, crossed with a gated dispatch site.

// The IEnumerable / IEnumerable<T> idiom - a non-generic interface
// declared beside its generic twin. Both mint `Src.cs::ISource`, the
// second is dropped, and the variance stamp rides on the DROPPED one:
// `seen[id]` returns before the stamp is ever evaluated.
//
// Variance is the signal that disarms the equality gate, so losing it
// re-arms an invariant-only filter over a covariant family and drops the
// covariant implementor - the exact P1 the variance guard was added to
// prevent, reached through a different door.
func TestResolveCSharpInterfaceDispatch_NonGenericTwinKeepsVarianceStamp(t *testing.T) {
	const withTwin = `namespace App {
    public class Animal { }
    public class Dog : Animal { }
    public interface ISource { void Reset(); }
    public interface ISource<out T> { T Get(); }
    public class DogSource : ISource<Dog> { public Dog Get() { return null; } }
    public class AnimalSource : ISource<Animal> { public Animal Get() { return null; } }
    public class Flow {
        private readonly ISource<Animal> _src;
        public Flow(ISource<Animal> s) { _src = s; }
        public Animal Pull() { return _src.Get(); }
    }
}`

	// The control is the same source with the non-generic twin removed.
	// It already passed before this fix, which is what makes the twin
	// case a collision problem rather than a variance problem.
	const withoutTwin = `namespace App {
    public class Animal { }
    public class Dog : Animal { }
    public interface ISource<out T> { T Get(); }
    public class DogSource : ISource<Dog> { public Dog Get() { return null; } }
    public class AnimalSource : ISource<Animal> { public Animal Get() { return null; } }
    public class Flow {
        private readonly ISource<Animal> _src;
        public Flow(ISource<Animal> s) { _src = s; }
        public Animal Pull() { return _src.Get(); }
    }
}`

	for _, tc := range []struct {
		name string
		src  string
	}{
		{"control: variant interface alone", withoutTwin},
		{"non-generic twin present", withTwin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := buildCSharpResolverGraph(t, map[string]string{"Src.cs": tc.src})
			New(g).ResolveAll()

			const callerID = "Src.cs::Flow.Pull"
			bindFieldReceiverCall(t, g, callerID, "_src", "Src.cs::ISource.Get")
			ResolveCSharpInterfaceDispatch(g)

			// The EXACT set, not membership. A gate's failure mode is
			// removing a valid target, and an assertion that only asks
			// whether one good target is present cannot observe a
			// removal - it stays green while the set shrinks around it.
			assert.ElementsMatch(t, []string{
				"Src.cs::AnimalSource.Get",
				"Src.cs::DogSource.Get",
			}, dispatchTargets(g, callerID),
				"ISource<out T> makes an ISource<Dog> assignable to an ISource<Animal> slot, so both implementors stay reachable")
		})
	}
}

// Same-file partial parts. Each part is its own declaration with its own
// base list, so the duplicate guard - which counts base entries within
// ONE base list - sees an unambiguous single IBox in each and lets both
// stamp. They are the same type: both mint `Boxes.cs::Store`, the second
// declaration is dropped whole, and the one surviving implements edge
// carries whichever closure was written first.
//
// The gate then paints that single closure onto every same-named member
// of the type and filters both Put overloads against it. Which closure
// wins is source-order dependent, so the fan-out is not merely wrong, it
// is unstable under reordering.
func TestResolveCSharpInterfaceDispatch_SameFilePartialPartsKeepBothOverloads(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Boxes.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { void Put(T item); }
    public partial class Store : IBox<Widget> { public void Put(Widget w) { } }
    public partial class Store : IBox<Crate> { public void Put(Crate c) { } }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> b) { _box = b; }
        public void Pull(Crate c) { _box.Put(c); }
    }
}`,
	})
	New(g).ResolveAll()

	const callerID = "Boxes.cs::Flow.Pull"
	bindFieldReceiverCall(t, g, callerID, "_box", "Boxes.cs::IBox.Put")
	ResolveCSharpInterfaceDispatch(g)

	// One type reaching IBox through two closures cannot be filtered on
	// either of them, so both overloads stay. The declaration that lost
	// the ID race is exactly the evidence that would have been needed to
	// filter correctly - which is why its absence has to disarm the gate
	// rather than license it.
	assert.ElementsMatch(t, []string{
		"Boxes.cs::Store.Put",
		"Boxes.cs::Store.Put_L6",
	}, dispatchTargets(g, callerID),
		"a type whose parts close IBox twice must keep its whole fan-out")
}
