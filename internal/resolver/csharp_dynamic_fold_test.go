package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/parser"
)

// Every parser-side fold must be visible to the alias refusal: a fold
// the comparable-forms expansion does not know is a silent fan-out
// suppressor. The forms now derive from the parser's own declarative
// table; this pins the contract in case the two sides ever diverge
// again.
func TestCSharpAliasComparableForms_CoverEveryParserFold(t *testing.T) {
	for spelling, folded := range parser.CSharpBCLKeywordFolds {
		forms := csharpAliasComparableForms(spelling)
		assert.Contains(t, forms, spelling, "the canonical alias name itself")
		assert.Contains(t, forms, folded, "the parser folds %q to %q - the refusal must know that form", spelling, folded)
		assert.Contains(t, csharpAliasComparableForms("@"+spelling), folded,
			"the verbatim spelling of %q folds identically", spelling)
	}
}

// `global using @dynamic = App.Crate;` - the parser canonicalizes the
// @dynamic spelling to object (dynamic erases to object), but the
// resolver's alias comparable-forms table lacked the dynamic->object
// hop, so the global-alias refusal missed the stamped object form: the
// receiver stamped a closure no implementor matched and the ENTIRE
// fan-out was suppressed (round-5 finding 8).
func TestResolveCSharpInterfaceDispatch_DynamicAliasFoldRefuses(t *testing.T) {
	g := buildCSharpResolverGraph(t, map[string]string{
		"Global.cs": `global using @dynamic = App.Crate;
`,
		"dynamic_types.cs": `namespace App {
    public class Crate { }
    public interface IBox<T> { void Put(T item); }
    public class CrateBox : IBox<Crate> { public void Put(Crate item) { } }
    public class Flow {
        private readonly IBox<@dynamic> _box;
        public Flow(IBox<@dynamic> box) { _box = box; }
        public void Pull(Crate item) { _box.Put(item); }
    }
}`,
	})
	New(g).ResolveAll()

	const callerID = "dynamic_types.cs::Flow.Pull"
	bindMemberCallAtLine(t, g, callerID, "Put", "dynamic_types.cs::IBox.Put")
	ResolveCSharpInterfaceDispatch(g)

	targets := dispatchTargets(g, callerID)
	hasCrate := false
	for _, to := range targets {
		if to == "dynamic_types.cs::CrateBox.Put" {
			hasCrate = true
		}
	}
	assert.True(t, hasCrate,
		"the object-folded @dynamic stamp names a global alias - the refusal must catch the folded form and keep the fan-out, got %v", targets)
}
