package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// odooField mints a field node the way the Python extractor does:
// "<classID>#field:<name>".
func odooField(g *graph.Graph, classID, name string) string {
	id := classID + odooFieldIDSep + name
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindField, Name: name, FilePath: classID, Language: "python",
	})
	return id
}

// odooXMLIDStub mints an `ref="..."` placeholder the way the XML
// extractor does.
func odooXMLIDStub(g *graph.Graph, from, xmlID string, kind graph.EdgeKind) *graph.Edge {
	return odooStub(g, from, odooXMLIDStubPrefix+xmlID, kind, odooXMLVia,
		map[string]any{"odoo_xml_id": xmlID})
}

// An `ir.model.access` row names its model through the external ID the
// ORM mints, which no XML file declares.
func TestResolveOdooRefs_ImplicitModelXMLIDBindsToClass(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "sale/order.py::SaleOrder", "sale.order")
	g.AddNode(&graph.Node{
		ID: "odoo::record::sale.access_sale_order", Kind: graph.KindResource,
		Name: "access_sale_order", Language: "odoo_xml",
		Meta: map[string]any{"odoo_xml_id": "sale.access_sale_order"},
	})
	odooXMLIDStub(g, "odoo::record::sale.access_sale_order", "base.model_sale_order", graph.EdgeReferences)

	require.Positive(t, ResolveOdooRefs(g))

	e := odooFindEdge(g, graph.EdgeReferences, "odoo::record::sale.access_sale_order", "sale/order.py::SaleOrder")
	require.NotNil(t, e, "model_<name> must bind to the class declaring the model")
	assert.Equal(t, SynthOdoo, e.Meta[MetaSynthesizedBy])
}

// A same-module rule writes the implicit ID bare, with no module prefix.
func TestResolveOdooRefs_ImplicitModelXMLIDWithoutModulePrefix(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "his/emr.py::HisEmrDoc", "his.emr.doc")
	g.AddNode(&graph.Node{
		ID: "odoo::record::his.rule", Kind: graph.KindResource, Name: "rule",
		Language: "odoo_xml", Meta: map[string]any{"odoo_xml_id": "his.rule"},
	})
	odooXMLIDStub(g, "odoo::record::his.rule", "model_his_emr_doc", graph.EdgeReferences)

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeReferences, "odoo::record::his.rule", "his/emr.py::HisEmrDoc"))
}

// An Odoo model is a name: a rule written against it governs every addon
// that declares it, so the implicit ID fans out the same way `_inherit`
// does.
func TestResolveOdooRefs_ImplicitModelXMLIDFansOut(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "a/order.py::SaleOrder", "sale.order")
	odooModelClass(g, "b/order.py::SaleOrderB", "sale.order")
	g.AddNode(&graph.Node{
		ID: "odoo::record::sale.rule", Kind: graph.KindResource, Name: "rule",
		Language: "odoo_xml", Meta: map[string]any{"odoo_xml_id": "sale.rule"},
	})
	odooXMLIDStub(g, "odoo::record::sale.rule", "base.model_sale_order", graph.EdgeReferences)

	ResolveOdooRefs(g)

	for _, want := range []string{"a/order.py::SaleOrder", "b/order.py::SaleOrderB"} {
		assert.NotNil(t, odooFindEdge(g, graph.EdgeReferences, "odoo::record::sale.rule", want),
			"every class declaring the model must be bound (%s)", want)
	}
}

// The fan-out must survive a second pass: a materialised sibling carries
// the source edge's Meta, so recomputing it would collapse the fan-out.
func TestResolveOdooRefs_ImplicitXMLIDFanOutIsIdempotent(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "a/order.py::SaleOrder", "sale.order")
	odooModelClass(g, "b/order.py::SaleOrderB", "sale.order")
	g.AddNode(&graph.Node{
		ID: "odoo::record::sale.rule", Kind: graph.KindResource, Name: "rule",
		Language: "odoo_xml", Meta: map[string]any{"odoo_xml_id": "sale.rule"},
	})
	odooXMLIDStub(g, "odoo::record::sale.rule", "base.model_sale_order", graph.EdgeReferences)

	first := ResolveOdooRefs(g)
	before := 0
	for range g.EdgesByKind(graph.EdgeReferences) {
		before++
	}
	second := ResolveOdooRefs(g)
	after := 0
	for range g.EdgesByKind(graph.EdgeReferences) {
		after++
	}

	assert.Equal(t, first, second, "a second run must report the same resolution count")
	assert.Equal(t, before, after, "a second run must not add edges")
	assert.NotNil(t, odooFindEdge(g, graph.EdgeReferences, "odoo::record::sale.rule", "b/order.py::SaleOrderB"),
		"the fan-out sibling must survive a recompute")
}

func TestResolveOdooRefs_ImplicitFieldXMLIDBindsToField(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "base/country.py::ResCountry", "res.country")
	field := odooField(g, "base/country.py::ResCountry", "phone_code")
	g.AddNode(&graph.Node{
		ID: "odoo::record::base.rule", Kind: graph.KindResource, Name: "rule",
		Language: "odoo_xml", Meta: map[string]any{"odoo_xml_id": "base.rule"},
	})
	odooXMLIDStub(g, "odoo::record::base.rule", "base.field_res_country__phone_code", graph.EdgeReferences)

	ResolveOdooRefs(g)

	assert.NotNil(t, odooFindEdge(g, graph.EdgeReferences, "odoo::record::base.rule", field),
		"field_<model>__<name> must bind to the field node")
}

func TestResolveOdooRefs_ImplicitModuleXMLIDBindsToManifest(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "sale/order.py::SaleOrder", "sale.order")
	g.AddNode(&graph.Node{
		ID: "addons/module::odoo:payment_click@16.0.1.0.0", Kind: graph.KindModule,
		Name: "payment_click", FilePath: "addons/payment_click/__manifest__.py",
		Language: "python", Meta: map[string]any{"odoo_module": "payment_click"},
	})
	g.AddNode(&graph.Node{
		ID: "odoo::record::base.cat", Kind: graph.KindResource, Name: "cat",
		Language: "odoo_xml", Meta: map[string]any{"odoo_xml_id": "base.cat"},
	})
	odooXMLIDStub(g, "odoo::record::base.cat", "base.module_payment_click", graph.EdgeReferences)

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeReferences, "odoo::record::base.cat", "addons/module::odoo:payment_click@16.0.1.0.0"),
		"module_<addon> must bind to the addon's manifest module node")
}

// A same-named module from another ecosystem is not the addon.
func TestOdooImplicitXMLIDs_IgnoresForeignModuleNodes(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "module::npm:sale@1.0.0", Kind: graph.KindModule, Name: "sale",
	})
	assert.Empty(t, buildOdooImplicitXMLIDs(g).lookup("base.module_sale"))
}

// A declared record wins: the implicit index is only ever a fallback.
func TestResolveOdooRefs_DeclaredRecordBeatsImplicitReading(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "sale/order.py::SaleOrder", "sale.order")
	g.AddNode(&graph.Node{
		ID: "odoo::record::sale.model_sale_order", Kind: graph.KindResource,
		Name: "model_sale_order", Language: "odoo_xml",
		Meta: map[string]any{"odoo_xml_id": "sale.model_sale_order"},
	})
	odooXMLIDStub(g, "sale/order.py::SaleOrder", "sale.model_sale_order", graph.EdgeReferences)

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeReferences, "sale/order.py::SaleOrder", "odoo::record::sale.model_sale_order"),
		"an explicitly declared record must win over the synthesized reading")
}

// An implicit-looking ID with no model behind it must stay a placeholder
// rather than be invented into an edge.
func TestResolveOdooRefs_UnknownImplicitXMLIDStaysUnbound(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "sale/order.py::SaleOrder", "sale.order")
	e := odooXMLIDStub(g, "sale/order.py::SaleOrder", "base.model_does_not_exist", graph.EdgeReferences)

	ResolveOdooRefs(g)
	assert.Equal(t, odooXMLIDStubPrefix+"base.model_does_not_exist", e.To)
}

func TestOdooImplicitXMLID_ShapeTest(t *testing.T) {
	for _, id := range []string{
		"base.model_res_country", "model_res_country",
		"base.field_res_country__name", "base.module_sale",
	} {
		assert.True(t, odooIsImplicitXMLID(id), "%s is an ORM-minted ID", id)
	}
	for _, id := range []string{"sale.view_order", "view_order", "base.action_res_users"} {
		assert.False(t, odooIsImplicitXMLID(id), "%s is a declared record", id)
	}
	assert.Empty(t, (*odooImplicitXMLIDs)(nil).lookup("base.model_sale_order"))
}
