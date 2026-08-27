package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The gate store embeds graph.Store, and graph.CheckoutGrouped is not part
// of it — so without explicit forwarding the wrapper answers "no sibling
// checkouts" to every pass that asks its own store, and passes that filter
// their own candidates stop filtering. The gate still refuses the edges, so
// the graph stays correct and nothing fails; the cost is invisible.
func TestFrameworkRepoGateStore_RepublishesCheckoutGrouping(t *testing.T) {
	base := graph.New()
	base.SetCheckoutGroups(map[string]string{"local": "grp", "local@wt": "grp"})
	require.True(t, graph.HasSiblingCheckouts(base), "fixture must publish a grouping")

	gated := newFrameworkRepoGateStore(graph.New(), &frameworkRepoGate{}, "odoo", base)

	assert.True(t, graph.HasSiblingCheckouts(gated),
		"a pass asking its own store must see the workspace's checkout grouping")
	assert.True(t, graph.SiblingCheckouts(gated, "local", "local@wt"),
		"two checkouts of one repository must read as siblings through the wrapper")
	assert.False(t, graph.SiblingCheckouts(gated, "local", "odoo"),
		"unrelated repositories must not")
}

// The odooSiblingCache is built from whatever store the pass is handed. If
// that is the gate wrapper, it must come up active — an inert cache filters
// nothing and every cross-checkout candidate is built before being refused.
func TestOdooSiblingCache_ActiveThroughTheGateStore(t *testing.T) {
	base := graph.New()
	base.SetCheckoutGroups(map[string]string{"local": "grp", "local@wt": "grp"})
	gated := newFrameworkRepoGateStore(graph.New(), &frameworkRepoGate{}, "odoo", base)

	sc := newOdooSiblingCache(gated)
	require.True(t, sc.active, "sibling cache must be active behind the gate wrapper")

	kept := sc.keep("local/a.py::A", "sale.order", []string{
		"local/decl.py::A", "local@wt/decl.py::A", "odoo/decl.py::A",
	})
	assert.Equal(t, []string{"local/decl.py::A", "odoo/decl.py::A"}, kept,
		"the sibling checkout's duplicate must be dropped before an edge is built for it")
}

// A workspace with no worktree must not pay for any of this, and the
// wrapper must not invent a grouping the store does not publish.
func TestFrameworkRepoGateStore_NoGroupingStaysInert(t *testing.T) {
	base := graph.New()
	gated := &frameworkRepoGateStore{Store: graph.New(), gate: &frameworkRepoGate{}, pass: "odoo", siblingSrc: base}

	assert.False(t, gated.HasCheckoutGroups())
	assert.Empty(t, gated.CheckoutGroup("local"))
	assert.False(t, newOdooSiblingCache(gated).active)
}

// Declaring FnValuePlaceholderEdges makes every gated store satisfy the
// scanner interface, so it has to answer correctly for a store underneath
// that does not implement it. Returning nil would panic the caller's range.
func TestFrameworkRepoGateStore_FnValuePlaceholderFallbackIsSafe(t *testing.T) {
	base := graph.New()
	gated := &frameworkRepoGateStore{Store: storeWithoutFnValueScanner{base}, gate: &frameworkRepoGate{}, pass: "fn-value-callback"}

	seq := gated.FnValuePlaceholderEdges()
	require.NotNil(t, seq, "a nil sequence panics the caller's range")
	count := 0
	for range seq {
		count++
	}
	assert.Zero(t, count)
}

// storeWithoutFnValueScanner hides the optional capability the way a
// third-party Store would.
type storeWithoutFnValueScanner struct{ graph.Store }

func (s storeWithoutFnValueScanner) FnValuePlaceholderEdges() {}

// The forwarding must reach the real index when the store has one — that is
// the whole point, and an assertion that silently fails leaves the pass on
// its wide fallback with nothing to show for it.
func TestFrameworkRepoGateStore_ForwardsFnValuePlaceholderIndex(t *testing.T) {
	base := graph.New()
	gated := newFrameworkRepoGateStore(base, &frameworkRepoGate{}, "fn-value-callback", base)
	if _, wrapped := gated.(*frameworkRepoGateStore); !wrapped {
		t.Skip("nothing narrows this pass, so the store is unwrapped and forwarding is moot")
	}
	_, ok := gated.(graph.FnValuePlaceholderScanner)
	assert.True(t, ok, "a gated store must still expose the fn-value placeholder index")
}
