package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// A static-form extension call passes the receiver as its first argument,
// so the arity window must NOT discount the `this` slot. That distinction
// rides on the receiver's spelling, which the extractor stamps only when
// nothing else could type the receiver — and a namespace-qualified
// receiver took the chain-walker branch instead, so it arrived carrying
// no spelling at all.
//
// The binder then read `Lib.BagExt.Add(bag)` as extension form, subtracted
// a slot the argument list had actually filled, and bound the two-parameter
// overload. `BagExt.Add(bag)` — the same call, written unqualified — bound
// the correct one, so the two spellings disagreed.
func TestResolveCSharpExtension_QualifiedStaticFormPicksSameOverload(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Ext.cs": `using System;
namespace Lib {
    public interface IBag { }
    public class Opts { }
    public static class BagExt {
        public static IBag Add(this IBag bag) { return bag; }
        public static IBag Add(this IBag bag, Action<Opts> configure) { return bag; }
    }
}`,
		"Caller.cs": `using Lib;
namespace App {
    public class Runner {
        public void Simple(IBag b) { BagExt.Add(b); }
        public void Qualified(IBag b) { Lib.BagExt.Add(b); }
        public void QualifiedTwo(IBag b) { Lib.BagExt.Add(b, x => { }); }
    }
}`,
	})
	New(g).ResolveAll()

	const oneParam = "Ext.cs::BagExt.Add"
	const twoParam = "Ext.cs::BagExt.Add_L7"

	assert.Equal(t, oneParam, namedCallTarget(t, g, "Caller.cs::Runner.Simple", "Add"),
		"control: the unqualified static form already worked")
	assert.Equal(t, oneParam, namedCallTarget(t, g, "Caller.cs::Runner.Qualified", "Add"),
		"the same call written namespace-qualified must reach the same overload")
	assert.Equal(t, twoParam, namedCallTarget(t, g, "Caller.cs::Runner.QualifiedTwo", "Add"),
		"a qualified static-form call with a real second argument is the two-parameter overload")
}
