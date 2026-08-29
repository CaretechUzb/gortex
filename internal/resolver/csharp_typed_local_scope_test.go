package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// An explicitly-TYPED local dies with its block exactly like a var
// local, but the typed lookup (tenv/builtin maps) was function-wide:
// `{ int TLBagExt = 1; }` kept answering "int" for every later site in
// the method, so the static-form call after the block resolved its
// receiver as int and landed on unresolved::*.Add (round-5 finding 2 -
// the var twin of the committed nested-block pin, which passes because
// var locals ride the offset-aware scope records).
func TestResolveCSharpExtension_ExpiredTypedLocalKeepsStaticForm(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `using System.Collections.Generic;
namespace Lib {
    public sealed class TLBag { }
    public static class TLBagExt {
        public static IEnumerable<int> Add(this TLBag bag, int value) { return new int[0]; }
        public static IEnumerable<int> Add(this TLBag bag, int value, int extra) { return new int[0]; }
    }
}`,
		"Caller.cs": `using System.Collections.Generic;
using Lib;
namespace App {
    public class Use {
        public IEnumerable<int> ExpiredTypedLocal(TLBag bag) {
            { int TLBagExt = 1; _ = TLBagExt; }
            return TLBagExt.Add(bag, 5);
        }
        public IEnumerable<int> Control(TLBag bag) {
            return TLBagExt.Add(bag, 5);
        }
    }
}`,
	})
	New(g).ResolveAll()

	const twoParam = "Ext.cs::TLBagExt.Add"

	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Use.Control", "Add"),
		"control: no local anywhere - the static form binds")
	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Use.ExpiredTypedLocal", "Add"),
		"the typed local's block has closed before the call - the receiver is the static class again, not an int")
}
