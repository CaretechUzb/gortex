package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func odooExtract(t *testing.T, src string) *parser.ExtractionResult {
	t.Helper()
	res, err := NewPythonExtractor().Extract("addons/sale/models/sale_order.py", []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return res
}

func odooClassNode(t *testing.T, res *parser.ExtractionResult, name string) *graph.Node {
	t.Helper()
	for _, n := range res.Nodes {
		if n != nil && n.Kind == graph.KindType && n.Name == name {
			return n
		}
	}
	t.Fatalf("no class node %q", name)
	return nil
}

func odooFieldNode(res *parser.ExtractionResult, name string) *graph.Node {
	for _, n := range res.Nodes {
		if n != nil && n.Kind == graph.KindField && n.Name == name {
			return n
		}
	}
	return nil
}

func odooEdge(res *parser.ExtractionResult, kind graph.EdgeKind, to string) *graph.Edge {
	for _, e := range res.Edges {
		if e != nil && e.Kind == kind && e.To == to {
			return e
		}
	}
	return nil
}

const odooSaleOrder = `
from odoo import models, fields, api

class SaleOrder(models.Model):
    _name = "sale.order"
    _description = "Sales Order"

    name = fields.Char(required=True)
    partner_id = fields.Many2one("res.partner", string="Customer")
    line_ids = fields.One2many(comodel_name="sale.order.line", inverse_name="order_id")
    amount_total = fields.Monetary(compute="_compute_amount")

    def _compute_amount(self):
        pass
`

func TestOdooModel_TagsClassIdentity(t *testing.T) {
	res := odooExtract(t, odooSaleOrder)
	n := odooClassNode(t, res, "SaleOrder")

	if got := n.Meta["odoo_model"]; got != "sale.order" {
		t.Errorf("odoo_model = %v, want sale.order", got)
	}
	if got := n.Meta["odoo_model_kind"]; got != "model" {
		t.Errorf("odoo_model_kind = %v, want model", got)
	}
	if got := n.Meta["odoo_description"]; got != "Sales Order" {
		t.Errorf("odoo_description = %v", got)
	}
}

// The model's table name is Odoo's documented derivation from _name, so
// Odoo models join every existing table query for free.
func TestOdooModel_EmitsTableEdge(t *testing.T) {
	res := odooExtract(t, odooSaleOrder)
	e := odooEdge(res, graph.EdgeModelsTable, ormTableNodeID("sale_order"))
	if e == nil {
		t.Fatal("no EdgeModelsTable to sale_order")
	}
	if got := e.Meta["orm"]; got != "odoo" {
		t.Errorf("orm = %v, want odoo", got)
	}
	if got := e.Meta["model_name"]; got != "sale.order" {
		t.Errorf("model_name = %v", got)
	}
}

// The relational graph is the highest-value output: it is invisible in
// plain Python because the comodel is a call argument.
func TestOdooModel_RelationalFieldsBecomeReferences(t *testing.T) {
	res := odooExtract(t, odooSaleOrder)

	for _, want := range []string{"res.partner", "sale.order.line"} {
		e := odooEdge(res, graph.EdgeReferences, odooPlaceholderPrefix+want)
		if e == nil {
			t.Fatalf("no comodel reference to %q", want)
		}
		if e.Meta["via"] != odooModelVia {
			t.Errorf("comodel edge to %q missing via tag: %v", want, e.Meta)
		}
	}

	// Positional and comodel_name= forms must both be read.
	if f := odooFieldNode(res, "partner_id"); f == nil || f.Meta["odoo_comodel"] != "res.partner" {
		t.Errorf("positional comodel not captured: %+v", f)
	}
	if f := odooFieldNode(res, "line_ids"); f == nil || f.Meta["odoo_comodel"] != "sale.order.line" {
		t.Errorf("comodel_name kwarg not captured: %+v", f)
	}
}

// Python emits no class-body field nodes of its own, so these must be
// minted here or the fields are invisible.
func TestOdooModel_MintsFieldNodes(t *testing.T) {
	res := odooExtract(t, odooSaleOrder)

	name := odooFieldNode(res, "name")
	if name == nil {
		t.Fatal("no field node for name")
	}
	if got := name.Meta["odoo_field_type"]; got != "Char" {
		t.Errorf("odoo_field_type = %v, want Char", got)
	}
	if got := name.Meta["odoo_required"]; got != true {
		t.Errorf("odoo_required = %v, want true", got)
	}
	if e := odooEdge(res, graph.EdgeMemberOf, odooClassNode(t, res, "SaleOrder").ID); e == nil {
		t.Error("field nodes must be members of their class")
	}
	if f := odooFieldNode(res, "amount_total"); f == nil || f.Meta["odoo_compute"] != "_compute_amount" {
		t.Errorf("compute= not captured: %+v", f)
	}
}

// A class carrying only _inherit extends an existing model in place, and
// takes that model's identity.
func TestOdooModel_InheritOnlyTakesParentIdentity(t *testing.T) {
	res := odooExtract(t, `
from odoo import models, fields

class SaleOrder(models.Model):
    _inherit = "sale.order"

    note = fields.Text()
`)
	n := odooClassNode(t, res, "SaleOrder")
	if got := n.Meta["odoo_model"]; got != "sale.order" {
		t.Errorf("odoo_model = %v, want sale.order", got)
	}
	// A self-referential extends edge would say nothing, so it is not emitted.
	if e := odooEdge(res, graph.EdgeExtends, odooPlaceholderPrefix+"sale.order"); e != nil {
		t.Error("an in-place extension must not emit a self extends edge")
	}
}

// Prototype inheritance: a new _name plus an _inherit really is a
// specialisation of another model.
func TestOdooModel_PrototypeInheritEmitsExtends(t *testing.T) {
	res := odooExtract(t, `
from odoo import models

class Wizard(models.TransientModel):
    _name = "sale.wizard"
    _inherit = ["mail.thread", "mail.activity.mixin"]
`)
	n := odooClassNode(t, res, "Wizard")
	if got := n.Meta["odoo_model_kind"]; got != "transient" {
		t.Errorf("odoo_model_kind = %v, want transient", got)
	}
	for _, want := range []string{"mail.thread", "mail.activity.mixin"} {
		e := odooEdge(res, graph.EdgeExtends, odooPlaceholderPrefix+want)
		if e == nil {
			t.Fatalf("no extends edge to %q (list _inherit form)", want)
		}
		if e.Meta["odoo_link"] != "inherit" {
			t.Errorf("edge to %q: odoo_link = %v", want, e.Meta["odoo_link"])
		}
	}
}

// _inherits is delegation, which is composition rather than specialisation.
func TestOdooModel_InheritsIsComposition(t *testing.T) {
	res := odooExtract(t, `
from odoo import models, fields

class ProductProduct(models.Model):
    _name = "product.product"
    _inherits = {"product.template": "product_tmpl_id"}

    product_tmpl_id = fields.Many2one("product.template", required=True)
`)
	e := odooEdge(res, graph.EdgeComposes, odooPlaceholderPrefix+"product.template")
	if e == nil {
		t.Fatal("no composes edge for _inherits delegation")
	}
	if got := e.Meta["odoo_delegate_field"]; got != "product_tmpl_id" {
		t.Errorf("odoo_delegate_field = %v", got)
	}
}

// An abstract model is a mixin with no table of its own.
func TestOdooModel_AbstractHasNoTable(t *testing.T) {
	res := odooExtract(t, `
from odoo import models, fields

class Mixin(models.AbstractModel):
    _name = "my.mixin"
    flag = fields.Boolean()
`)
	if got := odooClassNode(t, res, "Mixin").Meta["odoo_model_kind"]; got != "abstract" {
		t.Errorf("odoo_model_kind = %v, want abstract", got)
	}
	if e := odooEdge(res, graph.EdgeModelsTable, ormTableNodeID("my_mixin")); e != nil {
		t.Error("an abstract model must not claim a table")
	}
}

// Detection is two-factor precisely so a same-named class in an
// unrelated codebase is not mistaken for an Odoo model.
func TestOdooModel_RequiresBothFactors(t *testing.T) {
	// Qualified Odoo base, but no Odoo attribute or field.
	res := odooExtract(t, `
class NotOdoo(models.Model):
    def helper(self):
        return 1
`)
	if got := odooClassNode(t, res, "NotOdoo").Meta["odoo_model"]; got != nil {
		t.Errorf("a base name alone must not mark a model, got %v", got)
	}

	// Odoo attributes, but the base is not an Odoo base.
	res = odooExtract(t, `
class AlsoNot(SomethingElse):
    _name = "looks.odoo"
`)
	if got := odooClassNode(t, res, "AlsoNot").Meta["odoo_model"]; got != nil {
		t.Errorf("attributes alone must not mark a model, got %v", got)
	}
}

// The bare `Model` name is far too common to key on; only the qualified
// form counts.
func TestOdooModel_UnqualifiedBaseIsIgnored(t *testing.T) {
	res := odooExtract(t, `
from odoo.models import Model
from odoo import fields

class Bare(Model):
    _name = "bare.thing"
    x = fields.Char()
`)
	if got := odooClassNode(t, res, "Bare").Meta["odoo_model"]; got != nil {
		t.Errorf("an unqualified base must not be detected, got %v", got)
	}
}
