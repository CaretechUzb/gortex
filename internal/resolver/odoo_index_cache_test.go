package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// odooSiblingCache replaced a per-edge filter with a per-(repo, key) one.
// These pin the properties that swap depends on; the cost it was made for
// is not assertable at fixture scale, so nothing here measures time.

// A cached verdict must equal the uncached one for every asking
// repository — the whole point is that the answer does not depend on how
// many times it is asked.
func TestOdooSiblingCache_KeepMatchesTheUncachedFilter(t *testing.T) {
	g := siblingGraph(t)
	targets := []string{"core/c.py::X", "local-bench/a.py::X", "local/b.py::X"}

	uncached := func(fromID string) []string {
		asking := graph.RepoPrefixOfID(fromID)
		var kept []string
		for _, target := range targets {
			if graph.SiblingCheckouts(g, asking, graph.RepoPrefixOfID(target)) {
				continue
			}
			kept = append(kept, target)
		}
		return kept
	}

	sc := newOdooSiblingCache(g)
	for _, from := range []string{
		"local/caller.py::C", "local-bench/caller.py::C", "addons/caller.py::C",
	} {
		want := uncached(from)
		// Asked twice: the second answer comes from the cache and must
		// be the same one.
		assert.Equal(t, want, sc.keep(from, "sale.order", targets), "first ask: %s", from)
		assert.Equal(t, want, sc.keep(from, "sale.order", targets), "cached ask: %s", from)
	}

	assert.Equal(t, []string{"core/c.py::X", "local/b.py::X"},
		sc.keep("local/caller.py::C", "sale.order", targets),
		"the asking checkout's own copy stays, its worktree's copy goes")
	assert.Equal(t, []string{"core/c.py::X", "local-bench/a.py::X"},
		sc.keep("local-bench/caller.py::C", "sale.order", targets),
		"the rule is symmetric")
}

// The cache keys on (asking repo, lookup key). Two keys sharing an asking
// repo must not serve each other's verdicts.
func TestOdooSiblingCache_KeyIsPartOfTheCacheKey(t *testing.T) {
	sc := newOdooSiblingCache(siblingGraph(t))
	assert.Equal(t, []string{"core/a.py::X"},
		sc.keep("local/c.py::C", "sale.order", []string{"core/a.py::X", "local-bench/a.py::X"}))
	assert.Equal(t, []string{"core/b.py::Y"},
		sc.keep("local/c.py::C", "sale.order.line", []string{"core/b.py::Y", "local-bench/b.py::Y"}),
		"a second key must be filtered on its own list, not served the first's")
}

// declares is what retires fan-out siblings, so it must agree with keep
// exactly: a target the binder would refuse to create today is one
// retirement must be willing to remove.
func TestOdooSiblingCache_DeclaresAgreesWithKeep(t *testing.T) {
	sc := newOdooSiblingCache(siblingGraph(t))
	targets := []string{"core/c.py::X", "local-bench/a.py::X", "local/b.py::X"}
	const from = "local/caller.py::C"

	assert.True(t, sc.declares(from, "sale.order", targets, "local/b.py::X"))
	assert.True(t, sc.declares(from, "sale.order", targets, "core/c.py::X"))
	assert.False(t, sc.declares(from, "sale.order", targets, "local-bench/a.py::X"),
		"a target in a sibling checkout is not a declaration this edge may keep")
	assert.False(t, sc.declares(from, "sale.order", targets, "core/gone.py::X"),
		"a target that left the graph is not declared")

	for _, target := range sc.keep(from, "sale.order", targets) {
		assert.True(t, sc.declares(from, "sale.order", targets, target),
			"everything keep admits must be declared: %s", target)
	}
}

// Without a published grouping the cache is inert: no filtering, and the
// caller's slice comes straight back. This is the common workspace and it
// must not pay for the feature.
func TestOdooSiblingCache_InertWithoutGrouping(t *testing.T) {
	sc := newOdooSiblingCache(graph.New())
	require.False(t, sc.active)
	targets := []string{"core/c.py::X", "local-bench/a.py::X", "local/b.py::X"}
	assert.Equal(t, targets, sc.keep("local/caller.py::C", "sale.order", targets))
	assert.True(t, sc.declares("local/caller.py::C", "sale.order", targets, "local-bench/a.py::X"),
		"with nothing grouped, every target is still a real declaration")
}

// The nil cache is the zero-cost path for callers that have no pass in
// flight; it must not panic and must group nothing.
func TestOdooSiblingCache_NilIsSafe(t *testing.T) {
	var sc *odooSiblingCache
	assert.False(t, sc.siblings("local", "local-bench"))
	targets := []string{"local-bench/a.py::X"}
	assert.Equal(t, targets, sc.keep("local/c.py::C", "k", targets))
}
