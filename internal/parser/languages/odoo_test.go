package languages

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestOdooDetection(t *testing.T) {
	reg := parser.NewRegistry()
	RegisterAll(reg)
	for _, tc := range []struct{ file, src, want string }{
		{"a.xml", `<?xml version="1.0"?><odoo><record id="v" model="ir.ui.view"/></odoo>`, "odoo_xml"},
		{"a.xml", `<!-- <odoo> --><beans xmlns="http://www.springframework.org/schema/beans"/>`, "spring"},
		{"a.xml", `<openerp><data/></openerp>`, "odoo_xml"},
		{"a.xml", `<templates><t t-name="web.Card"/></templates>`, "odoo_xml"},
		{"ir.model.access.csv", "id,name,model_id:id,perm_read\nx,X,model_x,1\n", "odoo_csv"},
	} {
		if got, _ := reg.DetectLanguageContent(tc.file, []byte(tc.src)); got != tc.want {
			t.Errorf("%s: got %s want %s", tc.src, got, tc.want)
		}
	}
	for _, tc := range []struct{ file, src string }{{"a.xml", `<root><!-- <odoo> --></root>`}, {"report.csv", "id,name\nx,X\n"}, {"stock.move.csv", "name,amount\nx,1\n"}} {
		if got, _ := reg.DetectLanguageContent(tc.file, []byte(tc.src)); strings.HasPrefix(got, "odoo_") {
			t.Errorf("false Odoo detection: %s", tc.file)
		}
	}
}

func TestOdooPythonSpecifications(t *testing.T) {
	src := `from odoo import models, api, fields as f
class Wizard(models.TransientModel):
    _name = 'demo.wizard'
    _description = 'Demo wizard'
    _auto = False
    _check_company_auto = True
    _sql_constraints = [('unique_name', 'UNIQUE(name)', 'Unique')]
    name = f.Char(required=True, translate=True, index=True)
    tags = f.Many2many(comodel_name='demo.tag', relation='demo_tag_rel', column1='wizard_id', column2='tag_id')
    choice = f.Selection(selection_add=[('new','New')], ondelete={'new':'cascade'})
    @api.constrains('name')
    def _check_name(self): pass
    @api.onchange('name')
    def _onchange_name(self): pass
class Extension(models.Model):
    _inherit = 'demo.wizard'
class Abstract(models.AbstractModel):
    _name = 'demo.mixin'
class Dynamic(models.Model):
    _name = f'demo.{suffix}'
`
	r, err := NewPythonExtractor().Extract("demo/models/wizard.py", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*graph.Node{}
	for _, n := range r.Nodes {
		byID[n.ID] = n
	}
	wizard := byID["demo/models/wizard.py::Wizard"]
	if wizard.Meta["odoo_model"] != "demo.wizard" {
		t.Fatalf("missing model: %+v", wizard)
	}
	spec := wizard.Meta["odoo_spec"].(map[string]string)
	if spec["_auto"] != "False" || spec["_sql_constraints"] == "" {
		t.Fatal("model specification not retained")
	}
	field := byID[wizard.ID+".tags"]
	if field == nil || field.Meta["odoo_comodel"] != "demo.tag" {
		t.Fatal("aliased relational field not indexed")
	}
	for _, method := range []string{"_check_name", "_onchange_name"} {
		n := byID[wizard.ID+"."+method]
		b, _ := json.Marshal(n.Meta["odoo_refs"])
		if !strings.Contains(string(b), "demo.wizard/name") {
			t.Errorf("decorator lost for %s: %s", method, b)
		}
	}
	if n := byID["demo/models/wizard.py::Dynamic"]; n.Meta["odoo_model"] != nil {
		t.Fatal("dynamic f-string became a static model")
	}
	plain, err := NewPythonExtractor().Extract("plain.py", []byte("class Plain:\n    _name = 'ordinary'\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range plain.Nodes {
		if n.Meta["framework"] == "odoo" {
			t.Fatal("plain Python mislabeled")
		}
	}
}

func TestOdooXMLAndCSVSpecifications(t *testing.T) {
	src := `<odoo><data noupdate="1"><record id="rule" model="ir.rule"><field name="model_id" ref="model_demo"/><field name="groups" eval="[(4, ref('base.group_user'))]"/><field name="domain_force">[('active', '=', True)]</field></record><record id="cron" model="ir.cron"><field name="code">model.run()</field></record><record id="mail" model="mail.template"><field name="body_html" type="html"><div><t t-call="demo.message"/></div></field></record></data></odoo>`
	r, err := (&OdooDataExtractor{}).Extract("demo/data.xml", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var rule, cron *graph.Node
	for _, n := range r.Nodes {
		if n.Name == "rule" {
			rule = n
		}
		if n.Name == "cron" {
			cron = n
		}
	}
	if rule == nil || cron == nil || rule.Meta["odoo_noupdate"] != "1" {
		t.Fatal("records or noupdate lost")
	}
	b, _ := json.Marshal(rule.Meta)
	if !strings.Contains(string(b), "base.group_user") || !strings.Contains(string(b), "domain_force") {
		t.Fatal("rule specification lost")
	}
	b, _ = json.Marshal(cron.Meta)
	if strings.Contains(string(b), "base.group_user") {
		t.Fatal("sibling reference leakage")
	}
	if _, err = (&OdooDataExtractor{}).Extract("broken.xml", []byte(`<odoo><record></odoo>`)); err == nil {
		t.Fatal("malformed XML silently accepted")
	}
	csvSrc := "id,name,model_id/id,perm_read\naccess,\"Quoted,\nname\",model_demo,1\n"
	r, err = (&OdooDataExtractor{CSV: true}).Extract("demo/ir.model.access.csv", []byte(csvSrc))
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Nodes) != 2 || r.Nodes[1].StartLine != 2 {
		t.Fatal("CSV quoted multiline record lost")
	}
	if _, err = (&OdooDataExtractor{CSV: true}).Extract("demo/ir.model.access.csv", []byte("id,na\"me\nx,y\n")); err == nil {
		t.Fatal("malformed CSV header accepted")
	}
}

func TestOdooRoutes(t *testing.T) {
	r, err := NewPythonExtractor().Extract("demo/controllers/main.py", []byte(`from odoo import http
class Controller(http.Controller):
    @http.route(['/demo/<model("res.partner"):partner>', '/demo'], type='http', auth='user', methods=['GET'], csrf=False)
    def show(self, partner=None): pass
`))
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, n := range r.Nodes {
		if n.Meta["odoo_kind"] == "route" {
			count++
			found := false
			for _, e := range r.Edges {
				if e.From == n.ID && e.To == "demo/controllers/main.py::Controller.show" {
					found = true
				}
			}
			if !found {
				t.Fatal("route not attached to handler")
			}
		}
	}
	if count != 2 {
		t.Fatalf("got %d routes", count)
	}
}
