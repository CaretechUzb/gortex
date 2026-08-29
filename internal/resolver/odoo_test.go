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

// The fan-out siblings are the one family odooRebind cannot recompute —
// they carry no placeholder of their own. That left them outside the
// pass's full-recompute contract: deleting one of several classes
// declaring a model left its sibling edge pointing at a node that is gone,
// and file-scoped cleanup could not reach it either, because a sibling's
// FilePath is the SOURCE file rather than the vanished target's.
func TestResolveOdooRefs_FanOutRetiredWhenDeclaringClassIsDeleted(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "a/order.py::SaleOrder", "sale.order")
	odooModelClass(g, "b/order.py::SaleOrderB", "sale.order")
	odooModelClass(g, "c/w.py::Wizard", "sale.wizard")
	odooModelStub(g, "c/w.py::Wizard", "sale.order", graph.EdgeExtends)

	ResolveOdooRefs(g)
	require.NotNil(t, odooFindEdge(g, graph.EdgeExtends, "c/w.py::Wizard", "b/order.py::SaleOrderB"),
		"precondition: the sibling edge must exist before the class is deleted")

	// The addon is deleted; the class node goes with its file.
	g.EvictFile("b/order.py::SaleOrderB")
	ResolveOdooRefs(g)

	assert.Nil(t, odooFindEdge(g, graph.EdgeExtends, "c/w.py::Wizard", "b/order.py::SaleOrderB"),
		"the sibling edge must be retired with its target")
	assert.NotNil(t, odooFindEdge(g, graph.EdgeExtends, "c/w.py::Wizard", "a/order.py::SaleOrder"),
		"the surviving declaration must stay bound")
}

// The quieter half of the same gap: the target node is still in the graph,
// it just no longer declares the model.
func TestResolveOdooRefs_FanOutRetiredWhenClassStopsDeclaringModel(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "a/order.py::SaleOrder", "sale.order")
	odooModelClass(g, "b/order.py::SaleOrderB", "sale.order")
	odooModelClass(g, "c/w.py::Wizard", "sale.wizard")
	odooModelStub(g, "c/w.py::Wizard", "sale.order", graph.EdgeExtends)

	ResolveOdooRefs(g)
	require.NotNil(t, odooFindEdge(g, graph.EdgeExtends, "c/w.py::Wizard", "b/order.py::SaleOrderB"))

	// The class is edited to extend a different model.
	odooModelClass(g, "b/order.py::SaleOrderB", "sale.other")
	ResolveOdooRefs(g)

	assert.Nil(t, odooFindEdge(g, graph.EdgeExtends, "c/w.py::Wizard", "b/order.py::SaleOrderB"),
		"a class that no longer declares the model must not keep its sibling edge")
}

// Retirement must not fire on a steady state: re-running the pass with
// nothing changed keeps every sibling.
func TestResolveOdooRefs_FanOutSurvivesRerun(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "a/order.py::SaleOrder", "sale.order")
	odooModelClass(g, "b/order.py::SaleOrderB", "sale.order")
	odooModelClass(g, "c/w.py::Wizard", "sale.wizard")
	odooModelStub(g, "c/w.py::Wizard", "sale.order", graph.EdgeExtends)

	ResolveOdooRefs(g)
	ResolveOdooRefs(g)
	ResolveOdooRefs(g)

	for _, want := range []string{"a/order.py::SaleOrder", "b/order.py::SaleOrderB"} {
		assert.NotNil(t, odooFindEdge(g, graph.EdgeExtends, "c/w.py::Wizard", want),
			"a steady-state rerun must keep the fan-out (%s)", want)
	}
}

// A test module addresses its helpers by climbing out of the addon's
// `static/src` root — `@web/../tests/helpers/utils` is
// `web/static/tests/helpers/utils.js`. Odoo's asset loader resolves the
// specifier as a path relative to that root, so the `..` is meaningful
// rather than decorative; matched literally it can never hit a file,
// because no real path carries the segment.
func TestResolveOdooRefs_ImportEscapingStaticSrcBindsToFile(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "addons/web/static/tests/helpers/utils.js", Kind: graph.KindFile,
		Name: "utils.js", FilePath: "addons/web/static/tests/helpers/utils.js",
		Language: "javascript",
		Meta:     map[string]any{"odoo_js_module": true, "odoo_js_addon": "web"},
	})
	g.AddNode(&graph.Node{
		ID: "addons/sale/static/tests/t.js", Kind: graph.KindFile, Name: "t.js",
		FilePath: "addons/sale/static/tests/t.js", Language: "javascript",
		Meta: map[string]any{"odoo_js_module": true, "odoo_js_addon": "sale"},
	})
	odooStub(g, "addons/sale/static/tests/t.js",
		odooJSModuleStubPrefix+"@web/../tests/helpers/utils", graph.EdgeImports, odooJSVia,
		map[string]any{"odoo_js_import": "@web/../tests/helpers/utils"})

	ResolveOdooRefs(g)

	assert.NotNil(t,
		odooFindEdge(g, graph.EdgeImports, "addons/sale/static/tests/t.js",
			"addons/web/static/tests/helpers/utils.js"),
		"an import escaping static/src must bind to the file it names")
}

// The escape is resolved, not merely stripped: climbing past the addon
// root names nothing, and inventing a binding there would be worse than
// leaving the import unresolved.
func TestResolveOdooRefs_ImportEscapingPastTheAddonStaysUnbound(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "addons/web/static/tests/helpers/utils.js", Kind: graph.KindFile,
		Name: "utils.js", FilePath: "addons/web/static/tests/helpers/utils.js",
		Language: "javascript",
		Meta:     map[string]any{"odoo_js_module": true, "odoo_js_addon": "web"},
	})
	e := odooStub(g, "addons/sale/static/tests/t.js",
		odooJSModuleStubPrefix+"@web/../../../etc/passwd", graph.EdgeImports, odooJSVia,
		map[string]any{"odoo_js_import": "@web/../../../etc/passwd"})

	ResolveOdooRefs(g)

	assert.Equal(t, odooJSModuleStubPrefix+"@web/../../../etc/passwd", e.To,
		"a specifier climbing out of the addon must stay a placeholder")
}

// Retirement must not strand a model whose primary edge the STORE
// removed.
//
// When the class holding targets[0] is evicted, graph-level dangling-edge
// cleanup drops the primary with it, so the next pass collects only the
// surviving sibling — which carries no placeholder and is therefore never
// recomputed. Retirement driven by "the pass did not recompute it" then
// deletes that sibling too, and the reference to the model disappears
// outright even though a class still declares it. Fan-outs are widest
// exactly where Odoo is densest, so on a real corpus this takes most of
// the graph with it; keying retirement to the edge's own model instead
// cannot rotate and cannot strand.
func TestResolveOdooRefs_FanOutSurvivesEvictionOfThePrimaryTarget(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "a/order.py::SaleOrder", "sale.order")
	odooModelClass(g, "b/order.py::SaleOrderB", "sale.order")
	odooModelClass(g, "c/w.py::Wizard", "sale.wizard")
	e := odooModelStub(g, "c/w.py::Wizard", "sale.order", graph.EdgeExtends)

	ResolveOdooRefs(g)
	require.Equal(t, "a/order.py::SaleOrder", e.To,
		"precondition: the lowest-sorting declaration is the primary")
	require.NotNil(t, odooFindEdge(g, graph.EdgeExtends, "c/w.py::Wizard", "b/order.py::SaleOrderB"),
		"precondition: the second declaration is a sibling")

	g.EvictFile("a/order.py::SaleOrder")
	ResolveOdooRefs(g)
	ResolveOdooRefs(g)

	assert.NotNil(t, odooFindEdge(g, graph.EdgeExtends, "c/w.py::Wizard", "b/order.py::SaleOrderB"),
		"the surviving declaration must stay linked after the primary's target is evicted")
}

// A partial run must bind against the WHOLE graph, not just the changed
// repository.
//
// Every Odoo family is a full recompute: an edge whose target is missing
// from the index is reset to its placeholder, which is how a reference to
// a deleted record un-binds itself. If the pass builds its indexes from
// the changed repository alone, it applies that verdict to every Odoo edge
// in the workspace — so indexing a second addon after the first silently
// resets the first's edges to placeholders while the records they name are
// still sitting in the graph.
func TestResolveOdooRefsScoped_DoesNotUnbindOutOfScopeRepos(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "odoo/sale/views/v.xml", Kind: graph.KindFile,
		FilePath: "odoo/sale/views/v.xml", Language: "odoo_xml",
	})
	g.AddNode(&graph.Node{
		ID: "odoo/odoo::record::sale.view_order", Kind: graph.KindResource,
		Name: "view_order", QualName: "sale.view_order",
		FilePath: "odoo/sale/views/v.xml", Language: "odoo_xml",
		Meta: map[string]any{"odoo_xml_id": "sale.view_order"},
	})
	e := odooStub(g, "odoo/sale/views/v.xml",
		odooXMLIDStubPrefix+"sale.view_order", graph.EdgeReferences, odooXMLVia,
		map[string]any{"odoo_xml_id": "sale.view_order"})

	ResolveOdooRefs(g)
	require.Equal(t, "odoo/odoo::record::sale.view_order", e.To,
		"precondition: a full run binds the reference")

	// A second repository is indexed. Its scoped pass must leave the
	// first repository's already-bound edge exactly as it found it.
	ResolveOdooRefsScoped(g, map[string]bool{"addons": true})

	assert.Equal(t, "odoo/odoo::record::sale.view_order", e.To,
		"a scoped run must not reset an out-of-scope edge to its placeholder")
}
