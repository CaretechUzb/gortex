package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func odooXML(t *testing.T, filePath, src string) *parser.ExtractionResult {
	t.Helper()
	res, err := NewOdooXMLExtractor().Extract(filePath, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return res
}

func odooNode(res *parser.ExtractionResult, id string) *graph.Node {
	for _, n := range res.Nodes {
		if n != nil && n.ID == id {
			return n
		}
	}
	return nil
}

func odooHasEdge(res *parser.ExtractionResult, kind graph.EdgeKind, to string) *graph.Edge {
	for _, e := range res.Edges {
		if e != nil && e.Kind == kind && e.To == to {
			return e
		}
	}
	return nil
}

func TestIsOdooXML(t *testing.T) {
	cases := map[string]bool{
		`<?xml version="1.0"?><odoo><record id="a" model="m"/></odoo>`: true,
		`<openerp><data/></openerp>`:                                   true,
		// Standalone QWeb asset templates have no <odoo> wrapper.
		`<templates xml:space="preserve"><t t-name="web.Thing"/></templates>`: true,
		`<mapper namespace="com.app.UserMapper"><select id="x"/></mapper>`:    false,
		`<beans><bean class="x"/></beans>`:                                    false,
		`<root><child/></root>`:                                               false,
	}
	for src, want := range cases {
		if got := IsOdooXML([]byte(src)); got != want {
			t.Errorf("IsOdooXML(%.40s…) = %v, want %v", src, got, want)
		}
	}
}

// A misrouted plain-XML file must degrade to just the file node rather
// than failing or inventing records.
func TestOdooXML_NonOdooYieldsOnlyFileNode(t *testing.T) {
	res := odooXML(t, "addons/sale/data/x.xml", `<root><child ref="a"/></root>`)
	if len(res.Nodes) != 1 || res.Nodes[0].Kind != graph.KindFile {
		t.Errorf("expected only a file node, got %d nodes", len(res.Nodes))
	}
	if len(res.Edges) != 0 {
		t.Errorf("expected no edges, got %d", len(res.Edges))
	}
}

func TestOdooXML_RecordBindsToModel(t *testing.T) {
	res := odooXML(t, "addons/sale/views/sale_views.xml", `
<odoo>
  <record id="view_order_form" model="ir.ui.view">
    <field name="name">sale.order.form</field>
    <field name="model">sale.order</field>
  </record>
</odoo>`)

	n := odooNode(res, "odoo::record::sale.view_order_form")
	if n == nil {
		t.Fatal("no record node with the module-qualified external ID")
	}
	if n.Kind != graph.KindResource {
		t.Errorf("record node kind = %v, want resource", n.Kind)
	}
	if got := n.Meta["odoo_model"]; got != "ir.ui.view" {
		t.Errorf("odoo_model = %v, want ir.ui.view", got)
	}
	e := odooHasEdge(res, graph.EdgeReferences, odooPlaceholderPrefix+"ir.ui.view")
	if e == nil {
		t.Fatal("record must reference the Python model it configures")
	}
	// <field name="model">sale.order</field> carries its value as element
	// TEXT — the canonical way an Odoo view names the model it renders.
	if e := odooHasEdge(res, graph.EdgeReferences, odooPlaceholderPrefix+"sale.order"); e == nil {
		t.Fatal("the model named by <field name=\"model\"> text must be referenced")
	}
	if e.Meta["via"] != odooModelVia {
		t.Errorf("model reference must carry the odoo-model via tag, got %v", e.Meta["via"])
	}
}

// inherit_id is real view inheritance, not a plain reference.
func TestOdooXML_InheritIdIsExtends(t *testing.T) {
	res := odooXML(t, "addons/sale/views/sale_views.xml", `
<odoo>
  <record id="view_order_form_inherit" model="ir.ui.view">
    <field name="inherit_id" ref="sale.view_order_form"/>
  </record>
</odoo>`)
	if e := odooHasEdge(res, graph.EdgeExtends, odooXMLIDPlaceholder+"sale.view_order_form"); e == nil {
		t.Fatal("inherit_id must produce an extends edge")
	}
	if e := odooHasEdge(res, graph.EdgeReferences, odooXMLIDPlaceholder+"sale.view_order_form"); e != nil {
		t.Error("inherit_id must not ALSO be emitted as a plain reference")
	}
}

// Inside module `sale`, ref="view_x" means sale.view_x. Without the
// implicit prefix the same view carries two identities.
func TestOdooXML_ImplicitModulePrefix(t *testing.T) {
	res := odooXML(t, "addons/sale/views/menus.xml", `
<odoo>
  <menuitem id="menu_sale_root" action="action_orders" parent="base.menu_root"/>
</odoo>`)

	if odooNode(res, "odoo::menu::sale.menu_sale_root") == nil {
		t.Fatal("menu node must carry the module-qualified ID")
	}
	// Bare reference gains the module prefix...
	if e := odooHasEdge(res, graph.EdgeReferences, odooXMLIDPlaceholder+"sale.action_orders"); e == nil {
		t.Error("a bare ref must be qualified with the owning module")
	}
	// ...while an already-dotted one is left alone.
	if e := odooHasEdge(res, graph.EdgeReferences, odooXMLIDPlaceholder+"base.menu_root"); e == nil {
		t.Error("an already-qualified ref must not be re-prefixed")
	}
}

func TestOdooXML_TemplateAndQWeb(t *testing.T) {
	res := odooXML(t, "addons/website/views/templates.xml", `
<odoo>
  <template id="layout" inherit_id="web.layout"/>
</odoo>`)
	if odooNode(res, "odoo::template::website.layout") == nil {
		t.Fatal("no template node")
	}
	if e := odooHasEdge(res, graph.EdgeExtends, odooXMLIDPlaceholder+"web.layout"); e == nil {
		t.Error("template inherit_id must produce an extends edge")
	}

	// Standalone QWeb assets carry an already-qualified t-name.
	res = odooXML(t, "addons/web/static/src/xml/thing.xml", `
<templates xml:space="preserve">
  <t t-name="web.Thing"><div/></t>
</templates>`)
	n := odooNode(res, "odoo::template::web.Thing")
	if n == nil {
		t.Fatal("no QWeb template node")
	}
	if got := n.Meta["odoo_template"]; got != "web.Thing" {
		t.Errorf("odoo_template = %v", got)
	}
}

// <function model= name=> invokes a Python method at data-load time.
func TestOdooXML_FunctionCall(t *testing.T) {
	res := odooXML(t, "addons/sale/data/init.xml", `
<odoo>
  <record id="post_init" model="ir.actions.server">
    <function model="sale.order" name="_post_install"/>
  </record>
</odoo>`)
	e := odooHasEdge(res, graph.EdgeCalls, odooMethodPlaceholder+"sale.order._post_install")
	if e == nil {
		t.Fatal("no call edge for <function>")
	}
	if e.Meta["via"] != odooXMLVia {
		t.Errorf("via = %v, want %v", e.Meta["via"], odooXMLVia)
	}
}

func TestOdooXML_EvalRef(t *testing.T) {
	res := odooXML(t, "addons/sale/security/rules.xml", `
<odoo>
  <record id="rule_a" model="ir.rule">
    <field name="groups" eval="[(4, ref('base.group_user'))]"/>
  </record>
</odoo>`)
	if e := odooHasEdge(res, graph.EdgeReferences, odooXMLIDPlaceholder+"base.group_user"); e == nil {
		t.Fatal("ref() inside eval= must be captured")
	}
}

// A malformed document must yield whatever decoded, never a hard failure.
func TestOdooXML_MalformedDegradesGracefully(t *testing.T) {
	res := odooXML(t, "addons/sale/views/broken.xml", `
<odoo>
  <record id="ok" model="sale.order"/>
  <record id="broken" model="x"
`)
	if odooNode(res, "odoo::record::sale.ok") == nil {
		t.Error("records decoded before the error must be kept")
	}
}

func TestOdooModuleFromPath(t *testing.T) {
	cases := map[string]string{
		"addons/sale/views/sale_views.xml":        "sale",
		"custom_addons/my_mod/data/x.xml":         "my_mod",
		"addons/web/static/src/xml/templates.xml": "web",
		"project/my_addon/views/v.xml":            "my_addon",
	}
	for in, want := range cases {
		if got := odooModuleFromPath(in); got != want {
			t.Errorf("odooModuleFromPath(%q) = %q, want %q", in, got, want)
		}
	}
}
