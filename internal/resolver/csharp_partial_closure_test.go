package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A partial class carries its two interface paths on two declarations.
// With the second fragment's base list dropped at node deduplication,
// the surviving Widget stamp read as the type's unique closure and the
// entire Dual family was filtered from the IBox<Crate> fan-out (round-5
// finding 5). With both fragments' base facts preserved, the closure
// walk sees the second path (ICrateBox -> IBox<Crate>) disagree with
// the direct Widget stamp and refuses to filter - the conservative
// fan-out keeps both Dual.Put overloads alongside PlainCrateBox.
func TestResolveCSharpInterfaceDispatch_PartialSecondPathKeepsTheFamily(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"partial.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { void Put(T item); }
    public class PlainCrateBox : IBox<Crate> { public void Put(Crate item) { } }
    public interface ICrateBox : IBox<Crate> { }
    public partial class Dual : IBox<Widget> { public void Put(Widget item) { } }
    public partial class Dual : ICrateBox { public void Put(Crate item) { } }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> box) { _box = box; }
        public void Pull(Crate item) { _box.Put(item); }
    }
}`,
	})
	New(g).ResolveAll()

	const callerID = "partial.cs::Flow.Pull"
	bindMemberCallAtLine(t, g, callerID, "Put", "partial.cs::IBox.Put")
	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	hasPlain, hasDual := false, false
	for _, to := range targets {
		switch {
		case to == "partial.cs::PlainCrateBox.Put":
			hasPlain = true
		case to == "partial.cs::Dual.Put" || to == "partial.cs::Dual.Put_L8":
			hasDual = true
		}
	}
	assert.True(t, hasPlain, "the plain implementor stays, got %v", targets)
	assert.True(t, hasDual,
		"Dual reaches IBox<Crate> through its second fragment - at minimum one Dual.Put must survive the fan-out, got %v", targets)
}
