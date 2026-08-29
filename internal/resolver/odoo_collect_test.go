package resolver

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// odooCollectFamilies replaced three independent walks of the edge store
// with one. Its claim is about how much it reads, and that is not
// observable from the edges it hands back — so these tests count the
// reads. Nothing here measures time: at fixture scale it would measure
// noise (see the cross-repo plan-pin note for the precedent).

// odooKindCounter records how many times each edge bucket is opened and
// how many rows the collector is handed.
type odooKindCounter struct {
	graph.Store
	opens map[graph.EdgeKind]int
	rows  int
}

func (s *odooKindCounter) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.opens[kind]++
	inner := s.Store.EdgesByKind(kind)
	return func(yield func(*graph.Edge) bool) {
		for e := range inner {
			s.rows++
			if !yield(e) {
				return
			}
		}
	}
}

func odooCollectFixture(t *testing.T) *odooKindCounter {
	t.Helper()
	g := graph.New()
	add := func(from, to string, kind graph.EdgeKind, via string) {
		g.AddEdge(&graph.Edge{
			From: from, To: to, Kind: kind,
			Meta: map[string]any{"via": via},
		})
	}
	// All three families ride EdgeReferences. That overlap is what made
	// the old per-family walk expensive: on the real workspace it is the
	// largest bucket in the store and it was streamed three times.
	add("a.py::A", "unresolved::odoo::model::sale.order", graph.EdgeReferences, odooModelVia)
	add("b.xml::B", "unresolved::odoo::xmlid::sale.view_order", graph.EdgeReferences, odooXMLVia)
	add("c.js::C", "unresolved::odoo::template::sale.OrderWidget", graph.EdgeReferences, odooJSVia)
	// Kinds only one family rides.
	add("a.py::A", "unresolved::odoo::model::res.partner", graph.EdgeComposes, odooModelVia)
	add("b.xml::B", "unresolved::odoo::method::sale.order._do", graph.EdgeCalls, odooXMLVia)
	add("c.js::C", "unresolved::odoo::jsmodule::@web/core/registry", graph.EdgeImports, odooJSVia)
	// Noise: a non-Odoo edge, and an Odoo via tag riding a kind its own
	// family does not declare.
	add("d.go::D", "d.go::E", graph.EdgeCalls, "")
	add("a.py::A", "a.py::Z", graph.EdgeCalls, odooModelVia)
	return &odooKindCounter{Store: g, opens: map[graph.EdgeKind]int{}}
}

func odooCollectAll(g graph.Store) (models, xmlIDs, js *odooFamily) {
	models = &odooFamily{via: odooModelVia, kinds: odooModelEdgeKinds}
	xmlIDs = &odooFamily{via: odooXMLVia, kinds: odooXMLEdgeKinds}
	js = &odooFamily{via: odooJSVia, kinds: odooJSEdgeKinds}
	odooCollectFamilies(g, nil, models, xmlIDs, js)
	return
}

func odooEdgeTargets(f *odooFamily) []string {
	out := make([]string, 0, len(f.edges))
	for _, e := range f.edges {
		out = append(out, e.To)
	}
	return out
}

// The point of the change: every kind any family rides is opened once,
// and each edge reaches the collector once — not once per family that
// happens to ride its kind.
func TestOdooCollectFamilies_ReadsEachBucketOnce(t *testing.T) {
	counter := odooCollectFixture(t)
	odooCollectAll(counter)

	// The union of the three families' kinds, not the sum: seven
	// distinct kinds across ten declarations.
	want := []graph.EdgeKind{
		graph.EdgeExtends, graph.EdgeComposes, graph.EdgeReferences,
		graph.EdgeCalls, graph.EdgeImports, graph.EdgeRendersChild,
		graph.EdgeOverrides,
	}
	require.Len(t, counter.opens, len(want), "opened buckets: %v", counter.opens)
	for _, kind := range want {
		assert.Equal(t, 1, counter.opens[kind], "bucket %s must be opened exactly once", kind)
	}

	assert.Equal(t, 8, counter.rows,
		"each edge is visited once; three walks would visit the three "+
			"EdgeReferences rows and the EdgeCalls rows again")
}

// Collecting once must hand each family exactly what its own walk did.
func TestOdooCollectFamilies_RoutesByViaAndKind(t *testing.T) {
	models, xmlIDs, js := odooCollectAll(odooCollectFixture(t))

	assert.ElementsMatch(t, []string{
		"unresolved::odoo::model::sale.order",
		"unresolved::odoo::model::res.partner",
	}, odooEdgeTargets(models))
	assert.ElementsMatch(t, []string{
		"unresolved::odoo::xmlid::sale.view_order",
		"unresolved::odoo::method::sale.order._do",
	}, odooEdgeTargets(xmlIDs))
	assert.ElementsMatch(t, []string{
		"unresolved::odoo::template::sale.OrderWidget",
		"unresolved::odoo::jsmodule::@web/core/registry",
	}, odooEdgeTargets(js))
}

// The via tag alone would widen the pass. bindOdooModels never streamed
// EdgeCalls, so an `odoo-model` tag riding one was invisible to it, and
// muxing the walk must not make it visible.
func TestOdooCollectFamilies_ViaAloneDoesNotAdmitAnEdge(t *testing.T) {
	models, xmlIDs, js := odooCollectAll(odooCollectFixture(t))
	for _, f := range []*odooFamily{models, xmlIDs, js} {
		assert.NotContains(t, odooEdgeTargets(f), "a.py::Z",
			"an odoo-model tag on EdgeCalls belongs to no family")
	}
}

// The scoped path is the incremental one. It reaches the edges through
// frameworkRepoEdges rather than the kind buckets, but the de-duplication
// is the same win: three overlapping calls built three overlapping
// slices, and one call builds one.
func TestOdooCollectFamilies_ScopedAsksForEachKindOnce(t *testing.T) {
	counter := odooCollectFixture(t)
	models := &odooFamily{via: odooModelVia, kinds: odooModelEdgeKinds}
	xmlIDs := &odooFamily{via: odooXMLVia, kinds: odooXMLEdgeKinds}
	js := &odooFamily{via: odooJSVia, kinds: odooJSEdgeKinds}
	odooCollectFamilies(counter, map[string]bool{"a.py": true}, models, xmlIDs, js)

	assert.Equal(t, 1, counter.opens[graph.EdgeReferences],
		"EdgeReferences is ridden by all three families and must still be "+
			"asked for once")
	assert.Equal(t, 8, counter.rows, "one pass over the edges, not three")
}
