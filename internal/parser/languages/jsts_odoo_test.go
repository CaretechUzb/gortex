package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func odooJS(t *testing.T, filePath, src string) *parser.ExtractionResult {
	t.Helper()
	res, err := NewJavaScriptExtractor().Extract(filePath, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return res
}

const odooJSPath = "addons/sale/static/src/js/order_widget.js"

// The @odoo-module header is mandatory from Odoo 15 on, which makes it a
// cheap and reliable gate — nothing runs without it.
func TestOdooJS_GateRequiresModuleMarker(t *testing.T) {
	res := odooJS(t, odooJSPath, `
import { registry } from "@web/core/registry";
registry.category("actions").add("thing", Thing);
`)
	if n := odooNode(res, odooJSPath); n != nil && n.Meta["odoo_js_module"] == true {
		t.Error("a file without the @odoo-module marker must not be treated as Odoo")
	}
	if e := odooHasEdge(res, graph.EdgeProvides, odooRegistryNodeID("actions", "thing")); e != nil {
		t.Error("no Odoo edges may be emitted without the marker")
	}
}

func TestOdooJS_TagsModuleAndAddon(t *testing.T) {
	res := odooJS(t, odooJSPath, `/** @odoo-module **/
import { registry } from "@web/core/registry";
`)
	n := odooNode(res, odooJSPath)
	if n == nil || n.Meta["odoo_js_module"] != true {
		t.Fatalf("file node not tagged: %+v", n)
	}
	if got := n.Meta["odoo_js_addon"]; got != "sale" {
		t.Errorf("odoo_js_addon = %v, want sale", got)
	}
}

// Addon-aliased imports carry no path, so nothing links the importer to
// the imported file without a placeholder.
func TestOdooJS_AddonAliasImport(t *testing.T) {
	res := odooJS(t, odooJSPath, `/** @odoo-module **/
import { registry } from "@web/core/registry";
import { Component } from "@odoo/owl";
import local from "./local";
`)
	for _, spec := range []string{"@web/core/registry", "@odoo/owl"} {
		e := odooHasEdge(res, graph.EdgeImports, odooJSModulePlaceholder+spec)
		if e == nil {
			t.Errorf("no placeholder import for %q", spec)
			continue
		}
		if e.Meta["via"] != odooJSVia {
			t.Errorf("import %q missing via tag", spec)
		}
	}
	// A relative import is ordinary JS and must be left to the generic resolver.
	if e := odooHasEdge(res, graph.EdgeImports, odooJSModulePlaceholder+"./local"); e != nil {
		t.Error("a relative import must not be claimed by the Odoo pass")
	}
}

// The provider and consumer of a service meet on one registry node, so
// the wiring needs no resolution pass.
func TestOdooJS_RegistryProviderAndServiceConsumerShareNode(t *testing.T) {
	res := odooJS(t, odooJSPath, `/** @odoo-module **/
import { registry } from "@web/core/registry";

export const ormService = { start() {} };
registry.category("services").add("orm", ormService);

function useOrm() {
    const orm = useService("orm");
    return orm;
}
`)
	regID := odooRegistryNodeID("services", "orm")
	if odooNode(res, regID) == nil {
		t.Fatalf("no registry node; nodes: %v", odooNodeIDs(res))
	}
	if e := odooHasEdge(res, graph.EdgeProvides, regID); e == nil {
		t.Error("registry .add must provide the registry node")
	}
	if e := odooHasEdge(res, graph.EdgeConsumes, regID); e == nil {
		t.Error("useService must consume the same registry node")
	}
}

func TestOdooJS_RegistryCategoryIsRead(t *testing.T) {
	res := odooJS(t, odooJSPath, `/** @odoo-module **/
registry.category("actions").add("my_action", MyAction);
`)
	n := odooNode(res, odooRegistryNodeID("actions", "my_action"))
	if n == nil {
		t.Fatalf("no registry node for the actions category; nodes: %v", odooNodeIDs(res))
	}
	if got := n.Meta["odoo_registry_category"]; got != "actions" {
		t.Errorf("odoo_registry_category = %v", got)
	}
	// A bare .add(...) with no registry.category(...) chain is some other
	// library's method and must not be claimed.
	res = odooJS(t, odooJSPath, `/** @odoo-module **/
someList.add("x", Y);
`)
	if len(res.Edges) > 0 {
		for _, e := range res.Edges {
			if e.Kind == graph.EdgeProvides {
				t.Error("a bare .add must not be read as a registry registration")
			}
		}
	}
}

// static template = "…" is the only link between an OWL class and the
// QWeb XML that renders it.
func TestOdooJS_OWLTemplateBindsToQWeb(t *testing.T) {
	res := odooJS(t, odooJSPath, `/** @odoo-module **/
import { Component } from "@odoo/owl";

export class OrderWidget extends Component {
    static template = "sale.OrderWidget";
}
`)
	cls := odooFindClassNodeByName(res, "OrderWidget")
	if cls == nil {
		t.Fatal("no class node")
	}
	if got := cls.Meta["odoo_owl_template"]; got != "sale.OrderWidget" {
		t.Errorf("odoo_owl_template = %v", got)
	}
	e := odooHasEdge(res, graph.EdgeRendersChild, odooTemplatePlaceholder+"sale.OrderWidget")
	if e == nil {
		t.Fatal("no renders_child edge to the QWeb template")
	}
	if e.From != cls.ID {
		t.Errorf("renders_child must start at the component class, got %q", e.From)
	}
}

func TestOdooJS_PatchEmitsOverrides(t *testing.T) {
	res := odooJS(t, odooJSPath, `/** @odoo-module **/
import { patch } from "@web/core/utils/patch";
import { ListRenderer } from "@web/views/list/list_renderer";

patch(ListRenderer.prototype, {
    setup() {},
    onCellClicked(record) {},
});
`)
	for _, m := range []string{"setup", "onCellClicked"} {
		e := odooHasEdge(res, graph.EdgeOverrides, odooJSOverridePlaceholder+"ListRenderer."+m)
		if e == nil {
			t.Errorf("no override edge for patched method %q", m)
			continue
		}
		if e.Meta["odoo_patch_target"] != "ListRenderer" {
			t.Errorf("%q: odoo_patch_target = %v", m, e.Meta["odoo_patch_target"])
		}
	}
}

// Pre-v15 addons still use the odoo.define / require wrapper.
func TestOdooJS_LegacyDefineAndRequire(t *testing.T) {
	res := odooJS(t, odooJSPath, `
odoo.define("web.Foo", function (require) {
    var Bar = require("web.Bar");
    return Bar;
});
`)
	n := odooNode(res, odooJSPath)
	if n == nil || n.Meta["odoo_js_legacy"] != true {
		t.Fatalf("legacy module not tagged: %+v", n)
	}
	if got := n.Meta["odoo_js_legacy_name"]; got != "web.Foo" {
		t.Errorf("odoo_js_legacy_name = %v, want web.Foo", got)
	}
	if e := odooHasEdge(res, graph.EdgeImports, odooJSModulePlaceholder+"web.Bar"); e == nil {
		t.Error("legacy require must produce an import placeholder")
	}
}

func TestOdooJSAddonFromPath(t *testing.T) {
	cases := map[string]string{
		"addons/sale/static/src/js/x.js":       "sale",
		"custom/my_mod/static/src/owl/comp.js": "my_mod",
		"src/plain.js":                         "",
	}
	for in, want := range cases {
		if got := odooJSAddonFromPath(in); got != want {
			t.Errorf("odooJSAddonFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// Odoo 16 migrated `odoo.define("web.env", …)` to a real ES module and
// left the dotted name behind only as an `alias=` in the header comment.
// Files still written against the legacy vocabulary go on importing that
// name, so without reading the annotation the two halves of the migration
// never meet and every such import stays unresolved.
func TestOdooJS_ModuleAliasDeclaresTheLegacyName(t *testing.T) {
	cases := map[string]string{
		`/** @odoo-module alias=web.public.widget */`:                  "web.public.widget",
		`/** @odoo-module alias=web.env default=false **/`:             "web.env",
		`/** @odoo-module alias = sale_expense.sale_order_many2one */`: "sale_expense.sale_order_many2one",
	}
	for header, want := range cases {
		res := odooJS(t, odooJSPath, header+"\nexport const x = 1;\n")
		n := odooNode(res, odooJSPath)
		if n == nil {
			t.Fatalf("no file node for %q", header)
		}
		if got := n.Meta["odoo_js_legacy_name"]; got != want {
			t.Errorf("%q: odoo_js_legacy_name = %v, want %v", header, got, want)
		}
	}
}

// A plain module declares no legacy name, and reading one out of an
// unrelated `alias` mention would collapse distinct modules onto one node.
func TestOdooJS_NoAliasLeavesNoLegacyName(t *testing.T) {
	res := odooJS(t, odooJSPath, "/** @odoo-module **/\nconst alias = someOther.alias;\n")
	n := odooNode(res, odooJSPath)
	if n == nil {
		t.Fatal("no file node")
	}
	if got, ok := n.Meta["odoo_js_legacy_name"]; ok {
		t.Errorf("odoo_js_legacy_name = %v, want unset", got)
	}
}

// An explicit odoo.define() is the stronger declaration of the pair and
// must win over an alias annotation in the same file.
func TestOdooJS_DefineWinsOverAlias(t *testing.T) {
	res := odooJS(t, odooJSPath, `/** @odoo-module alias=web.Stale */
odoo.define("web.Real", function (require) { return 1; });
`)
	n := odooNode(res, odooJSPath)
	if n == nil {
		t.Fatal("no file node")
	}
	if got := n.Meta["odoo_js_legacy_name"]; got != "web.Real" {
		t.Errorf("odoo_js_legacy_name = %v, want web.Real", got)
	}
}
