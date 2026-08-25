package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func odooModelClass(g *graph.Graph, id, model string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindType, Name: id, FilePath: id, Language: "python",
		Meta: map[string]any{"odoo_model": model},
	})
}

// odooStub mints a placeholder edge exactly as the extractors do: the
// key the binder resolves from lives in Meta, not in the target string,
// because the pass is a full recompute and must be able to re-derive (or
// un-derive) the target on every run.
func odooStub(g *graph.Graph, from, to string, kind graph.EdgeKind, via string, meta map[string]any) *graph.Edge {
	m := map[string]any{"via": via}
	for k, v := range meta {
		m[k] = v
	}
	e := &graph.Edge{From: from, To: to, Kind: kind, Meta: m}
	g.AddEdge(e)
	return e
}

func odooModelStub(g *graph.Graph, from, model string, kind graph.EdgeKind) *graph.Edge {
	return odooStub(g, from, odooModelStubPrefix+model, kind, odooModelVia,
		map[string]any{"odoo_model": model})
}

func odooFindEdge(g graph.Store, kind graph.EdgeKind, from, to string) *graph.Edge {
	for e := range g.EdgesByKind(kind) {
		if e != nil && e.From == from && e.To == to {
			return e
		}
	}
	return nil
}

func TestResolveOdooRefs_BindsInheritToDeclaringClass(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "sale/order.py::SaleOrder", "sale.order")
	odooModelClass(g, "custom/ext.py::SaleOrderExt", "sale.order.ext")
	odooModelStub(g, "custom/ext.py::SaleOrderExt", "sale.order", graph.EdgeExtends)

	n := ResolveOdooRefs(g)
	require.Positive(t, n)

	e := odooFindEdge(g, graph.EdgeExtends, "custom/ext.py::SaleOrderExt", "sale/order.py::SaleOrder")
	require.NotNil(t, e, "the _inherit placeholder must bind to the declaring class")
	assert.Equal(t, ConfidenceTyped, e.Confidence)
	assert.Equal(t, ProvenanceFramework, e.Meta[MetaProvenance])
	assert.Equal(t, SynthOdoo, e.Meta[MetaSynthesizedBy],
		"provenance must be the single odoo framework name")
}

// An Odoo model is a NAME, not a class: several addons routinely extend
// one _name, and binding to only one of them would hide the rest.
func TestResolveOdooRefs_ModelFanOutBindsEveryDeclaringClass(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "a/order.py::SaleOrder", "sale.order")
	odooModelClass(g, "b/order.py::SaleOrderB", "sale.order")
	odooModelClass(g, "c/w.py::Wizard", "sale.wizard")
	odooModelStub(g, "c/w.py::Wizard", "sale.order", graph.EdgeExtends)

	ResolveOdooRefs(g)

	for _, want := range []string{"a/order.py::SaleOrder", "b/order.py::SaleOrderB"} {
		assert.NotNil(t, odooFindEdge(g, graph.EdgeExtends, "c/w.py::Wizard", want),
			"every class declaring the model must be bound (%s)", want)
	}
}

func TestResolveOdooRefs_ComodelReference(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "base/partner.py::ResPartner", "res.partner")
	g.AddNode(&graph.Node{
		ID: "sale/order.py::SaleOrder#field:partner_id", Kind: graph.KindField,
		Name: "partner_id", Language: "python",
	})
	odooModelStub(g, "sale/order.py::SaleOrder#field:partner_id", "res.partner", graph.EdgeReferences)

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeReferences, "sale/order.py::SaleOrder#field:partner_id", "base/partner.py::ResPartner"),
		"a Many2one comodel must bind to the target model class")
}

// The XML record → Python model link is what makes a view navigable.
func TestResolveOdooRefs_XMLRecordBindsToModel(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "sale/order.py::SaleOrder", "sale.order")
	g.AddNode(&graph.Node{
		ID: "odoo::record::sale.view_order", Kind: graph.KindResource,
		Name: "view_order", Language: "odoo_xml",
		Meta: map[string]any{"odoo_xml_id": "sale.view_order"},
	})
	odooModelStub(g, "odoo::record::sale.view_order", "sale.order", graph.EdgeReferences)

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeReferences, "odoo::record::sale.view_order", "sale/order.py::SaleOrder"))
}

func TestResolveOdooRefs_XMLIDInheritance(t *testing.T) {
	g := graph.New()
	for _, id := range []string{"sale.view_order", "sale.view_order_inherit"} {
		g.AddNode(&graph.Node{
			ID: "odoo::record::" + id, Kind: graph.KindResource, Name: id,
			Language: "odoo_xml", Meta: map[string]any{"odoo_xml_id": id},
		})
	}
	odooStub(g, "odoo::record::sale.view_order_inherit",
		odooXMLIDStubPrefix+"sale.view_order", graph.EdgeExtends, odooXMLVia,
		map[string]any{"odoo_xml_id": "sale.view_order"})

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeExtends, "odoo::record::sale.view_order_inherit", "odoo::record::sale.view_order"),
		"inherit_id must bind to the parent view record")
}

// <function model= name=> must land on real Python code.
func TestResolveOdooRefs_XMLFunctionBindsToMethod(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "sale/order.py::SaleOrder", "sale.order")
	g.AddNode(&graph.Node{
		ID: "sale/order.py::SaleOrder._post_install", Kind: graph.KindMethod,
		Name: "_post_install", Language: "python",
	})
	g.AddNode(&graph.Node{
		ID: "odoo::record::sale.init", Kind: graph.KindResource, Name: "init",
		Language: "odoo_xml", Meta: map[string]any{"odoo_xml_id": "sale.init"},
	})
	odooStub(g, "odoo::record::sale.init",
		odooMethodStubPrefix+"sale.order._post_install", graph.EdgeCalls, odooXMLVia,
		map[string]any{"odoo_model": "sale.order", "odoo_method": "_post_install"})

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeCalls, "odoo::record::sale.init", "sale/order.py::SaleOrder._post_install"))
}

// The OWL component → QWeb template link is the JS↔XML bridge, and it
// only works because the XML binder runs before the JS binder.
func TestResolveOdooRefs_OWLTemplateBindsToQWebNode(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "odoo::template::sale.OrderWidget", Kind: graph.KindResource,
		Name: "sale.OrderWidget", Language: "odoo_xml",
		Meta: map[string]any{"odoo_template": "sale.OrderWidget", "odoo_xml_id": "sale.OrderWidget"},
	})
	g.AddNode(&graph.Node{
		ID: "addons/sale/static/src/js/w.js::OrderWidget", Kind: graph.KindType,
		Name: "OrderWidget", Language: "javascript",
	})
	odooStub(g, "addons/sale/static/src/js/w.js::OrderWidget",
		odooTemplateStubPrefix+"sale.OrderWidget", graph.EdgeRendersChild, odooJSVia,
		map[string]any{"odoo_template": "sale.OrderWidget"})

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeRendersChild, "addons/sale/static/src/js/w.js::OrderWidget", "odoo::template::sale.OrderWidget"),
		"an OWL component must bind to the QWeb template it names")
}

// @web/core/registry cannot be turned into a path by string surgery, so
// the join runs through the addon tag every Odoo JS file node carries.
func TestResolveOdooRefs_AddonAliasImportBindsToFile(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "addons/web/static/src/core/registry.js", Kind: graph.KindFile,
		Name: "registry.js", FilePath: "addons/web/static/src/core/registry.js",
		Language: "javascript",
		Meta:     map[string]any{"odoo_js_module": true, "odoo_js_addon": "web"},
	})
	g.AddNode(&graph.Node{
		ID: "addons/sale/static/src/js/w.js", Kind: graph.KindFile, Name: "w.js",
		FilePath: "addons/sale/static/src/js/w.js", Language: "javascript",
		Meta: map[string]any{"odoo_js_module": true, "odoo_js_addon": "sale"},
	})
	odooStub(g, "addons/sale/static/src/js/w.js",
		odooJSModuleStubPrefix+"@web/core/registry", graph.EdgeImports, odooJSVia,
		map[string]any{"odoo_js_import": "@web/core/registry"})

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeImports, "addons/sale/static/src/js/w.js", "addons/web/static/src/core/registry.js"),
		"an addon-aliased import must bind to the real file")
}

// Pre-v15 addons name their modules with odoo.define("web.core", …) and
// consume them with require("web.core"). The name is arbitrary — nothing
// about the path implies it — so the only possible join is against the
// name the defining file declared.
func TestResolveOdooRefs_LegacyDefineNameBindsToDefiningFile(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "addons/web/static/src/js/core.js", Kind: graph.KindFile, Name: "core.js",
		FilePath: "addons/web/static/src/js/core.js", Language: "javascript",
		Meta: map[string]any{
			"odoo_js_module": true, "odoo_js_addon": "web",
			"odoo_js_legacy": true, "odoo_js_legacy_name": "web.core",
		},
	})
	g.AddNode(&graph.Node{
		ID: "addons/sale/static/src/js/w.js", Kind: graph.KindFile, Name: "w.js",
		FilePath: "addons/sale/static/src/js/w.js", Language: "javascript",
		Meta: map[string]any{"odoo_js_module": true, "odoo_js_addon": "sale"},
	})
	odooStub(g, "addons/sale/static/src/js/w.js",
		odooJSModuleStubPrefix+"web.core", graph.EdgeImports, odooJSVia,
		map[string]any{"odoo_js_import": "web.core", "odoo_js_legacy": true})

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeImports, "addons/sale/static/src/js/w.js", "addons/web/static/src/js/core.js"),
		"a legacy require() must bind to the file that defined the module")
}

// Adding the legacy vocabulary must not shadow the modern one: the two
// key shapes (dotted name vs. slashed path) share one index.
func TestResolveOdooRefs_LegacyNameDoesNotShadowAddonAlias(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "addons/web/static/src/core/registry.js", Kind: graph.KindFile,
		Name: "registry.js", FilePath: "addons/web/static/src/core/registry.js",
		Language: "javascript",
		Meta: map[string]any{
			"odoo_js_module": true, "odoo_js_addon": "web",
			"odoo_js_legacy_name": "web.core",
		},
	})
	odooStub(g, "addons/sale/static/src/js/w.js",
		odooJSModuleStubPrefix+"@web/core/registry", graph.EdgeImports, odooJSVia,
		map[string]any{"odoo_js_import": "@web/core/registry"})

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeImports, "addons/sale/static/src/js/w.js", "addons/web/static/src/core/registry.js"),
		"an addon alias must still bind when the same file also declares a legacy name")
}

// @odoo/owl is the framework package, not an addon — leaving it unbound
// is correct, not a miss.
func TestResolveOdooRefs_FrameworkPackageImportStaysUnbound(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "addons/sale/static/src/js/w.js", Kind: graph.KindFile, Name: "w.js",
		FilePath: "addons/sale/static/src/js/w.js", Language: "javascript",
		Meta: map[string]any{"odoo_js_module": true, "odoo_js_addon": "sale"},
	})
	e := odooStub(g, "addons/sale/static/src/js/w.js",
		odooJSModuleStubPrefix+"@odoo/owl", graph.EdgeImports, odooJSVia,
		map[string]any{"odoo_js_import": "@odoo/owl"})

	ResolveOdooRefs(g)
	assert.Equal(t, odooJSModuleStubPrefix+"@odoo/owl", e.To,
		"the framework package has no file in the graph and must stay a placeholder")
}

// A dangling placeholder must not be invented into an edge.
func TestResolveOdooRefs_UnknownModelStaysUnbound(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "a/x.py::X", "a.x")
	e := odooModelStub(g, "a/x.py::X", "does.not.exist", graph.EdgeExtends)

	ResolveOdooRefs(g)
	assert.Equal(t, odooModelStubPrefix+"does.not.exist", e.To)
}

// The pass is a full recompute, so running it twice must not duplicate
// or drift.
func TestResolveOdooRefs_Idempotent(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "sale/order.py::SaleOrder", "sale.order")
	odooModelStub(g, "c/w.py::Wizard", "sale.order", graph.EdgeExtends)

	first := ResolveOdooRefs(g)
	before := 0
	for range g.EdgesByKind(graph.EdgeExtends) {
		before++
	}
	second := ResolveOdooRefs(g)
	after := 0
	for range g.EdgesByKind(graph.EdgeExtends) {
		after++
	}

	assert.Equal(t, first, second, "a second run must report the same resolution count")
	assert.Equal(t, before, after, "a second run must not add edges")
}

func TestResolveOdooRefs_NilGraph(t *testing.T) {
	assert.Zero(t, ResolveOdooRefs(nil))
}

// The registry gates Odoo on a single node marker because preflight
// markers are AND-ed; any one of the three halves must admit the pass.
func TestOdooNodeMarker_AnyHalfAdmits(t *testing.T) {
	cases := map[string]*graph.Node{
		"python model": {Language: "python", Meta: map[string]any{"odoo_model": "sale.order"}},
		"xml record":   {Language: "odoo_xml"},
		"owl js":       {Language: "javascript", Meta: map[string]any{"odoo_js_module": true}},
		"manifest":     {Language: "python", Meta: map[string]any{"odoo_module": "sale"}},
	}
	for name, n := range cases {
		assert.True(t, odooNodeMarker(n), "%s must admit the odoo pass", name)
	}
	assert.False(t, odooNodeMarker(&graph.Node{Language: "python"}))
	assert.False(t, odooNodeMarker(nil))
}

func TestOdooIsRegisteredAsASingleFramework(t *testing.T) {
	names := RegisteredFrameworkSynthesizerNames()
	assert.Contains(t, names, SynthOdoo)
	for _, n := range names {
		if n != SynthOdoo {
			assert.NotContains(t, n, "odoo",
				"Odoo must be one framework name, not split into sub-passes (%s)", n)
		}
	}
}
