package resolver

import (
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// buildOdooDecls replaced eleven whole-store node scans with two projected
// walks. Like odooCollectFamilies before it, its claim is about how much it
// reads, and that is invisible in the indexes it returns — so these tests
// count the reads. Nothing here measures time: at fixture scale that would
// measure noise (see the cross-repo plan-pin note for the precedent).

// odooNodeKindCounter records how many times each node bucket is opened.
// Every projection the builder can take — the FrameworkDeclNode sequence
// and the ID/name sequence alike — falls back through NodesByKind on a
// store that publishes neither capability, which an embedded graph.Store
// interface does not, so one override observes them all.
type odooNodeKindCounter struct {
	graph.Store
	opens map[graph.NodeKind]int
}

func (s *odooNodeKindCounter) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	s.opens[kind]++
	return s.Store.NodesByKind(kind)
}

// odooDeclFixture declares one of everything the six indexes are built
// from, so a walk that skipped a kind shows up as an empty index rather
// than as a merely smaller one.
func odooDeclFixture(t *testing.T) *odooNodeKindCounter {
	t.Helper()
	g := graph.New()

	// Python side: a class declaring `sale.order`, one of its methods,
	// and one of its fields.
	odooModelClass(g, "sale/order.py::SaleOrder", "sale.order")
	g.AddNode(&graph.Node{
		ID: "sale/order.py::SaleOrder.action_confirm", Kind: graph.KindMethod,
		Name: "action_confirm", Language: "python",
	})
	odooField(g, "sale/order.py::SaleOrder", "partner_id")

	// The addon itself, under the ID shape the manifest extractor mints.
	g.AddNode(&graph.Node{
		ID: "module::odoo:sale", Kind: graph.KindModule, Name: "sale",
	})

	// XML side: a record and a QWeb template.
	g.AddNode(&graph.Node{
		ID: "odoo::record::sale.view_order_form", Kind: graph.KindResource,
		Name: "view_order_form", Language: "odoo_xml",
		Meta: map[string]any{"odoo_xml_id": "sale.view_order_form"},
	})
	g.AddNode(&graph.Node{
		ID: "odoo::template::sale.OrderWidget", Kind: graph.KindResource,
		Name: "OrderWidget", Language: "odoo_xml",
		Meta: map[string]any{"odoo_template": "sale.OrderWidget"},
	})

	// JS side: a module file, a class in it, and a method on that class.
	g.AddNode(&graph.Node{
		ID: "sale/static/src/js/order.js", Kind: graph.KindFile,
		FilePath: "sale/static/src/js/order.js", Language: "javascript",
		Meta: map[string]any{"odoo_js_addon": "sale"},
	})
	g.AddNode(&graph.Node{
		ID: "sale/static/src/js/order.js::ListRenderer", Kind: graph.KindType,
		Name: "ListRenderer", Language: "javascript",
	})
	g.AddNode(&graph.Node{
		ID: "sale/static/src/js/order.js::ListRenderer.setup", Kind: graph.KindMethod,
		Name: "setup", Language: "javascript",
	})

	return &odooNodeKindCounter{Store: g, opens: map[graph.NodeKind]int{}}
}

// The point of the collapse: KindType was scanned four times and
// KindMethod twice, because each index builder opened its own.
func TestBuildOdooDecls_ReadsEachKindOnce(t *testing.T) {
	c := odooDeclFixture(t)

	buildOdooDecls(c)

	for _, kind := range []graph.NodeKind{
		graph.KindType, graph.KindResource, graph.KindFile,
		graph.KindMethod, graph.KindField, graph.KindModule,
	} {
		assert.Equalf(t, 1, c.opens[kind],
			"%s bucket must be opened exactly once per pass", kind)
	}
}

// Reading less must not index less. Every index the binders look through
// is asserted separately, because a walk that dropped one kind would
// still leave the other five correct.
func TestBuildOdooDecls_PopulatesEveryIndex(t *testing.T) {
	d := buildOdooDecls(odooDeclFixture(t))
	require.NotNil(t, d)

	assert.Equal(t, []string{"sale/order.py::SaleOrder"}, d.models["sale.order"],
		"model index: _name to declaring class")
	assert.NotEmpty(t, d.xmlIDs["sale.view_order_form"],
		"external-ID index: declared ref to record")
	assert.NotEmpty(t, d.xmlIDs["view_order_form"],
		"external-ID index: bare form of a declared ref")
	assert.NotEmpty(t, d.templates["sale.OrderWidget"],
		"template index: QWeb name to markup")
	assert.NotEmpty(t, d.jsModules["sale/js/order"],
		"JS module index: addon-relative specifier to file")
	assert.NotEmpty(t, d.modelMethods["sale.order.action_confirm"],
		"method index: <model>.<method> to Python method")
	assert.NotEmpty(t, d.jsMethods["ListRenderer.setup"],
		"JS method index: <Class>.<method> to patched method")
}

// The implicit index used to be built lazily, on the first reference that
// looked implicit, and a second time by the retirement predicate. It now
// rides the same walks, so it is populated whether or not anything asks.
func TestBuildOdooDecls_BuildsImplicitExternalIDsEagerly(t *testing.T) {
	d := buildOdooDecls(odooDeclFixture(t))
	require.NotNil(t, d.implicit)

	assert.Equal(t, []string{"sale/order.py::SaleOrder"},
		d.implicit.lookup("model_sale_order"), "implicit model ID")
	assert.NotEmpty(t, d.implicit.lookup("field_sale_order__partner_id"),
		"implicit field ID")
	assert.NotEmpty(t, d.implicit.lookup("module_sale"), "implicit module ID")
}

// A store holding no Odoo code at all must still cost only the walks, and
// the two identity-only walks that have nothing to join against are
// skipped outright.
func TestBuildOdooDecls_SkipsJoinWalksWithNoDeclarations(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "pkg/foo.go::Bar", Kind: graph.KindType, Name: "Bar", Language: "go",
	})
	c := &odooNodeKindCounter{Store: g, opens: map[graph.NodeKind]int{}}

	d := buildOdooDecls(c)

	assert.Empty(t, d.models)
	assert.Zero(t, c.opens[graph.KindField],
		"no declaring class means nothing for a field to join against")
	assert.Zero(t, c.opens[graph.KindMethod],
		"no declaring class and no JS class means nothing for a method to join against")
}
