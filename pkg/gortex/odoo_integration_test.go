package gortex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// End-to-end Odoo indexing.
//
// The unit tests cover each Odoo layer against its own extractor. This one
// runs the REAL pipeline over a real addon on disk — the walk, the .xml
// content sniff that has to pick the Odoo extractor over the generic one,
// every extractor, and the framework synthesizer — and asserts the edges
// that only exist once those layers agree. It is the test that would catch
// a Meta key renamed on one side of the extractor/resolver contract, which
// no single-package test can see.
func TestOdooEndToEnd(t *testing.T) {
	root := writeOdooAddon(t)

	eng, err := New(WithWorkers(2))
	require.NoError(t, err)
	t.Cleanup(func() { _ = eng.Close() })

	_, err = eng.Index(root)
	require.NoError(t, err)

	saleOrder := requireOdooModelClass(t, eng, "sale.order")
	resPartner := requireOdooModelClass(t, eng, "res.partner")

	t.Run("model identity", func(t *testing.T) {
		assert.Equal(t, "Sales Order", saleOrder.Meta["odoo_description"])
		assert.Equal(t, "model", saleOrder.Meta["odoo_model_kind"])
		// The EdgeModelsTable link is asserted in the extractor unit
		// test, not here: KindTable nodes ride the `sql` coverage
		// domain, which is off by default, so a default index strips
		// them for every ORM alike (stripSQLArtifacts).
	})

	// The ER graph: invisible in plain Python because the comodel is a
	// call argument.
	t.Run("relational field binds to comodel class", func(t *testing.T) {
		assert.True(t, hasEdgeTo(eng, resPartner.ID, graph.EdgeReferences),
			"Many2one('res.partner') must bind to the res.partner class")
	})

	// Requires the .xml sniff to have routed to the Odoo extractor AND
	// the model binder to have run.
	t.Run("xml record binds to python model", func(t *testing.T) {
		rec := requireNode(t, eng, "odoo::record::sale.view_order_form")
		assert.Equal(t, graph.KindResource, rec.Kind)
		// This record declares its model as element TEXT
		// (<field name="model">sale.order</field>), the canonical Odoo
		// idiom — so the binding proves both the char-data path and the
		// model index.
		assert.True(t, edgeExists(eng, rec.ID, saleOrder.ID, graph.EdgeReferences),
			"<field name=\"model\">sale.order</field> must bind to the SaleOrder class")
	})

	t.Run("view inheritance binds record to record", func(t *testing.T) {
		child := requireNode(t, eng, "odoo::record::sale.view_order_form_inherit")
		assert.True(t, edgeExists(eng, child.ID, "odoo::record::sale.view_order_form", graph.EdgeExtends),
			"inherit_id must bind to the parent view")
	})

	// The JS↔XML bridge, which only works because the XML binder runs
	// before the JS binder inside ResolveOdooRefs.
	t.Run("owl component binds to qweb template", func(t *testing.T) {
		widget := requireSymbol(t, eng, "OrderWidget", graph.KindType)
		assert.Equal(t, "sale.OrderWidget", widget.Meta["odoo_owl_template"])
		assert.True(t, edgeExists(eng, widget.ID, "odoo::template::sale.OrderWidget", graph.EdgeRendersChild),
			"static template must bind to the QWeb template node")
	})

	t.Run("manifest module and dependencies", func(t *testing.T) {
		mod := requireSymbol(t, eng, "sale", graph.KindModule)
		assert.Equal(t, "sale", mod.Meta["odoo_module"])
		assert.Equal(t, "Sales", mod.Meta["odoo_display_name"])
		assert.True(t, hasEdgeFrom(eng, mod.ID, graph.EdgeDependsOnModule),
			"depends must produce module dependency edges")
		assert.True(t, edgeExists(eng, mod.ID, "addons/sale/views/sale_views.xml", graph.EdgeReferences),
			"the manifest's data list must link the module to the real file")
	})

	t.Run("controller routes", func(t *testing.T) {
		found := false
		for n := range eng.store.NodesByKind(graph.KindContract) {
			if n == nil || n.Meta == nil {
				continue
			}
			// A route contract carries its framework label on the
			// nested contract_meta map, not at the top level.
			meta, _ := n.Meta["contract_meta"].(map[string]any)
			if meta == nil {
				continue
			}
			if fw, _ := meta["framework"].(string); fw == "odoo" {
				found = true
				assert.Equal(t, "http", meta["odoo_type"])
				assert.Equal(t, "public", meta["odoo_auth"])
				break
			}
		}
		assert.True(t, found, "@http.route must produce odoo-framework contracts")
	})
}

func requireOdooModelClass(t *testing.T, eng *Engine, model string) *graph.Node {
	t.Helper()
	for n := range eng.store.NodesByKind(graph.KindType) {
		if n == nil || n.Meta == nil {
			continue
		}
		if got, _ := n.Meta["odoo_model"].(string); got == model {
			return n
		}
	}
	t.Fatalf("no class tagged with odoo_model=%q", model)
	return nil
}

func requireNode(t *testing.T, eng *Engine, id string) *graph.Node {
	t.Helper()
	n := eng.GetSymbol(id)
	require.NotNil(t, n, "expected node %q", id)
	return n
}

func requireSymbol(t *testing.T, eng *Engine, name string, kind graph.NodeKind) *graph.Node {
	t.Helper()
	nodes := eng.FindSymbols(name, kind)
	require.NotEmpty(t, nodes, "expected a %s named %q", kind, name)
	return nodes[0]
}

func edgeExists(eng *Engine, from, to string, kind graph.EdgeKind) bool {
	for e := range eng.store.EdgesByKind(kind) {
		if e != nil && e.From == from && e.To == to {
			return true
		}
	}
	return false
}

func hasEdgeFrom(eng *Engine, from string, kind graph.EdgeKind) bool {
	for e := range eng.store.EdgesByKind(kind) {
		if e != nil && e.From == from {
			return true
		}
	}
	return false
}

func hasEdgeTo(eng *Engine, to string, kind graph.EdgeKind) bool {
	for e := range eng.store.EdgesByKind(kind) {
		if e != nil && e.To == to {
			return true
		}
	}
	return false
}

// writeOdooAddon lays out a minimal but realistic addon and returns the
// WORKSPACE root, so indexed paths carry the `addons/<module>/` segment a
// real Odoo checkout has — which is where the module name comes from.
func writeOdooAddon(t *testing.T) string {
	t.Helper()
	workspace := t.TempDir()
	root := filepath.Join(workspace, "addons", "sale")
	files := map[string]string{
		"__manifest__.py": `{
    "name": "Sales",
    "version": "17.0.1.0.0",
    "depends": ["base", "account"],
    "data": ["views/sale_views.xml"],
}
`,
		"models/sale_order.py": `from odoo import models, fields


class SaleOrder(models.Model):
    _name = "sale.order"
    _description = "Sales Order"

    name = fields.Char(required=True)
    partner_id = fields.Many2one("res.partner", string="Customer")
`,
		"models/res_partner.py": `from odoo import models, fields


class ResPartner(models.Model):
    _name = "res.partner"

    credit_limit = fields.Float()
`,
		"views/sale_views.xml": `<?xml version="1.0" encoding="utf-8"?>
<odoo>
  <record id="view_order_form" model="ir.ui.view">
    <field name="name">sale.order.form</field>
    <field name="model">sale.order</field>
  </record>
  <record id="view_order_form_inherit" model="ir.ui.view">
    <field name="inherit_id" ref="view_order_form"/>
  </record>
  <template id="OrderWidget"/>
</odoo>
`,
		"controllers/main.py": `from odoo import http


class SaleController(http.Controller):
    @http.route(['/shop'], type='http', auth='public', methods=['GET'])
    def shop(self, **kw):
        return None
`,
		"static/src/js/order_widget.js": `/** @odoo-module **/
import { Component } from "@odoo/owl";

export class OrderWidget extends Component {
    static template = "sale.OrderWidget";
}
`,
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o755))
		require.NoError(t, os.WriteFile(p, []byte(body), 0o644))
	}
	return workspace
}
