package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// Verbatim spellings in BASE-LIST position (an @-spelled alias, the
// interface name itself spelled `@IBox`) are legal respellings whose
// raw use bypassed alias and duplicate protection: the respelled path
// vanished, the surviving stamp read as the unique closure, and the
// whole dual-interface type was filtered out of the family fan-out
// (round-5 finding 7 end-to-end; the escape-decode quadrants are pinned
// at the extractor level).
func TestResolveCSharpInterfaceDispatch_RespelledBasesKeepTheFamily(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Rack.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { void Put(T item); }
    public class PlainCrateBox : IBox<Crate> { public void Put(Crate item) { } }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> box) { _box = box; }
        public void Pull(Crate item) { _box.Put(item); }
    }
}`,
		"Dual.cs": `using @BX = App.IBox<App.Crate>;
namespace App {
    public class DualA : @BX, IBox<Widget> {
        public void Put(Crate item) { }
        public void Put(Widget item) { }
    }
    public class DualB : @IBox<Crate>, IBox<Widget> {
        public void Put(Crate item) { }
        public void Put(Widget item) { }
    }
}`,
	})
	New(g).ResolveAll()

	const callerID = "Rack.cs::Flow.Pull"
	bindMemberCallAtLine(t, g, callerID, "Put", "Rack.cs::IBox.Put")
	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	hasA, hasB := false, false
	for _, to := range targets {
		switch to {
		case "Dual.cs::DualA.Put":
			hasA = true
		case "Dual.cs::DualB.Put":
			hasB = true
		}
	}
	assert.True(t, hasA, "the alias-based dual implementor must stay in the fan-out, got %v", targets)
	assert.True(t, hasB, "the verbatim-direct dual implementor must stay in the fan-out, got %v", targets)
}
