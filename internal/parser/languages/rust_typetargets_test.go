package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Canonicalisation stripped a tuple's parens and left the comma, and had
// no case at all for slices, arrays or raw pointers — so it produced
// targets like "unresolved::u8, u16" and "unresolved::[Item]" that can
// never resolve and pollute every unresolved-reference report.
func TestCanonicalizeRustTypeRef_RejectsNonTypeNames(t *testing.T) {
	assert.Equal(t, "Item", canonicalizeRustTypeRef("&[Item]"))
	assert.Equal(t, "Item", canonicalizeRustTypeRef("[Item; 4]"))
	assert.Equal(t, "c_void", canonicalizeRustTypeRef("*mut c_void"))
	assert.Equal(t, "Error", canonicalizeRustTypeRef("Box<dyn Error + Send + 'static>"))
	assert.Equal(t, "", canonicalizeRustTypeRef("(u8, u16)"),
		"a tuple is not one type name")
}

// A tuple names one type per slot, so each slot gets its own edge.
func TestRustCanonicalTypeRefs_ExpandsTuples(t *testing.T) {
	assert.Equal(t, []string{"Item", "Cfg"}, rustCanonicalTypeRefs("(Item, Cfg)"))
	assert.Equal(t, []string{"Cfg"}, rustCanonicalTypeRefs("(u8, Cfg)"),
		"primitives are skipped, named types are kept")
	assert.Equal(t, []string{"Item"}, rustCanonicalTypeRefs("Vec<Item>"))
}

func TestRsExtractor_TupleReturnEmitsOneEdgePerSlot(t *testing.T) {
	_, edges := rustNodesByID(t, `fn process() -> (Item, Cfg) { todo!() }`)
	got := map[string]int{}
	for _, e := range edges {
		if e.Kind == graph.EdgeReturns {
			got[e.To] = e.Meta["position"].(int)
		}
	}
	require.Contains(t, got, "unresolved::Item")
	require.Contains(t, got, "unresolved::Cfg")
	assert.Equal(t, 0, got["unresolved::Item"])
	assert.Equal(t, 1, got["unresolved::Cfg"])
}

// `impl Trait for &'a Buf` minted the method id `<file>::&'a Buf.show`
// and pointed its member_of at a node that never exists.
func TestRustImplReceiverName_UnwrapsNonNominalTargets(t *testing.T) {
	assert.Equal(t, "Buf", rustImplReceiverName("&'a Buf"))
	assert.Equal(t, "Buf", rustImplReceiverName("&mut Buf"))
	assert.Equal(t, "Item", rustImplReceiverName("[Item; 4]"))
	assert.Equal(t, "c_void", rustImplReceiverName("*mut c_void"))
	// The generic spelling is the established id shape and must survive.
	assert.Equal(t, "Replacer<M>", rustImplReceiverName("Replacer<M>"))
}

func TestRsExtractor_RefImplMethodBindsToRealOwner(t *testing.T) {
	byID, edges := rustNodesByID(t, `struct Buf;
impl Display for &'a Buf { fn show(&self) {} }
`)
	m := byID["probe.rs::Buf.show"]
	require.NotNil(t, m, "the method must hang off the named type")
	assert.Equal(t, "Buf", m.Meta["receiver"])

	var bound bool
	for _, e := range edges {
		if e.From == "probe.rs::Buf.show" && e.Kind == graph.EdgeMemberOf {
			assert.Equal(t, "probe.rs::Buf", e.To, "member_of must name a node that exists")
			bound = true
		}
	}
	assert.True(t, bound)
}
