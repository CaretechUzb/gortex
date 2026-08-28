package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// `receiver_name` is the only evidence that tells the binder a call is the
// STATIC form of an extension call — where the first argument fills the
// `this` slot rather than the receiver. The extractor refuses that stamp
// when a parameter or local declares the receiver's name, which is right:
// such a name is the local, not a static class.
//
// The refusal's SCOPE is wrong. `localNamesByOwner` is keyed on the
// enclosing function, so a local buried in a nested block vetoes the stamp
// for every call in the method — including calls the local cannot possibly
// bind at, because its block has already closed. The evidence vanishes, the
// binder reads the call as extension form, subtracts a `this` slot the
// argument list had actually filled, and the arity window lands one
// parameter too wide.
//
// Nothing here is generic or dispatch-related: this is the extension
// binder reading a two-argument call as three.
func TestResolveCSharpExtension_NestedBlockLocalKeepsStaticForm(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `namespace Lib {
    public class Bag { }
    public static class BagExt {
        public static void Add(this Bag b, int x) { }
        public static void Add(this Bag b, int x, int y) { }
    }
}`,
		"Caller.cs": `using Lib;
namespace App {
    public class Use {
        public void Shadowed(Bag bag) {
            if (bag != null) { var BagExt = 1; System.Console.WriteLine(BagExt); }
            BagExt.Add(bag, 5);
        }
        public void Control(Bag bag) {
            BagExt.Add(bag, 5);
        }
    }
}`,
	})
	New(g).ResolveAll()

	// Ext.cs:4 takes (this Bag, int) — two parameters, which is what a
	// static-form `BagExt.Add(bag, 5)` fills. Ext.cs:5 takes three.
	const twoParam = "Ext.cs::BagExt.Add"
	const threeParam = "Ext.cs::BagExt.Add_L5"

	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Use.Control", "Add"),
		"control: with no local anywhere in the method the static form already binds correctly")
	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Use.Shadowed", "Add"),
		"a local in a closed nested block shadows nothing at the call site and must not cost the call its static-form evidence")
}
