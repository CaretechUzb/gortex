package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A PROJECT-GLOBAL alias used in BASE-LIST position: alias collection is
// file-local, so `Dual : BX, IBox<Widget>` (with `global using BX =
// App.IBox<App.Crate>;` in another file) leaves an unresolved hierarchy
// target the resolver silently drops - Dual's IBox<Crate> path vanishes,
// the surviving Widget stamp reads as its unique closure, and the
// IBox<Crate> fan-out filters the whole type (round-5 finding 6).
//
// The correction is a refusal, not a resolution: an unresolved base
// whose name matches a project-global alias means the type cannot prove
// a unique closure, so its stamp is refused and the conservative
// fan-out keeps it.
func TestResolveCSharpInterfaceDispatch_GlobalAliasBaseKeepsTheFamily(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Global.cs": `global using BX = App.IBox<App.Crate>;
`,
		"global_base_types.cs": `namespace App {
    public class Crate { }
    public class Widget { }
    public interface IBox<T> { void Put(T item); }
    public class PlainCrateBox : IBox<Crate> { public void Put(Crate item) { } }
    public class Dual : BX, IBox<Widget> {
        public void Put(Crate item) { }
        public void Put(Widget item) { }
    }
    public class Flow {
        private readonly IBox<Crate> _box;
        public Flow(IBox<Crate> box) { _box = box; }
        public void Pull(Crate item) { _box.Put(item); }
    }
}`,
	})
	New(g).ResolveAll()

	const callerID = "global_base_types.cs::Flow.Pull"
	bindMemberCallAtLine(t, g, callerID, "Put", "global_base_types.cs::IBox.Put")
	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	hasPlain, hasDual := false, false
	for _, to := range targets {
		switch to {
		case "global_base_types.cs::PlainCrateBox.Put":
			hasPlain = true
		case "global_base_types.cs::Dual.Put", "global_base_types.cs::Dual.Put_L9":
			hasDual = true
		}
	}
	assert.True(t, hasPlain, "the plain implementor stays, got %v", targets)
	assert.True(t, hasDual,
		"Dual's BX path names a project-global alias the resolver cannot read - its stamp must refuse and the fan-out keep it, got %v", targets)
}
