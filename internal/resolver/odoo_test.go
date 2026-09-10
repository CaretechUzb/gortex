package resolver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func odooTestExtract(t *testing.T, g graph.Store, file, src, repo, workspace string) {
	t.Helper()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	language, _ := reg.DetectLanguageContent(file, []byte(src))
	ext, ok := reg.GetByLanguage(language)
	if !ok {
		t.Fatalf("no extractor for %s (%s)", file, language)
	}
	r, err := ext.Extract(file, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	// Exercise JSON round-trip: production SQLite does not retain concrete
	// []map types, unlike direct in-memory extraction tests.
	for _, n := range r.Nodes {
		n.RepoPrefix = repo
		n.WorkspaceID = workspace
		b, _ := json.Marshal(n.Meta)
		_ = json.Unmarshal(b, &n.Meta)
	}
	g.AddBatch(r.Nodes, r.Edges)
}
func odooTestEdge(t *testing.T, g graph.Store, from, to, relation string) {
	t.Helper()
	for _, e := range g.GetOutEdges(from) {
		if e.To == to && e.Meta["odoo_relation"] == relation {
			if e.Meta[MetaSynthesizedBy] != SynthOdoo {
				t.Fatal("missing provenance")
			}
			return
		}
	}
	t.Errorf("missing %s --%s--> %s", from, relation, to)
}

func TestOdooEndToEnd(t *testing.T) {
	g := graph.New()
	odooTestExtract(t, g, "base/__manifest__.py", `{'name':'Base','depends':[]}`, "core", "his")
	odooTestExtract(t, g, "clinic/__manifest__.py", `{'name':'Clinic','depends':['base'],'data':['views/patient.xml','security/ir.model.access.csv'],'assets':{'web.assets_backend':['clinic/static/src/widget.js']}}`, "local", "his")
	odooTestExtract(t, g, "base/models/partner.py", `from odoo import api, fields, models
class Partner(models.Model):
    _name = 'res.partner'
    name = fields.Char()
    def action_open(self): pass
`, "core", "his")
	odooTestExtract(t, g, "clinic/models/patient.py", `from odoo import api, fields, models
class Patient(models.Model):
    _name = 'clinic.patient'
    _inherit = ['res.partner']
    _inherits = {'res.partner': 'partner_id'}
    partner_id = fields.Many2one('res.partner', required=True, ondelete='cascade')
    total = fields.Float(compute='_compute_total', store=True)
    partner_name = fields.Char(related='partner_id.name')
    @api.depends('partner_id.name')
    def _compute_total(self):
        return self.env['res.partner'].search([])
    def action_open(self):
        return self.env.ref('clinic.patient_form')
`, "local", "his")
	odooTestExtract(t, g, "clinic/views/patient.xml", `<odoo><record id="patient_form" model="ir.ui.view"><field name="model">clinic.patient</field><field name="arch" type="xml"><form><field name="partner_id"/><button name="action_open" type="object"/></form></field></record><record id="patient_inherit" model="ir.ui.view"><field name="inherit_id" ref="patient_form"/><field name="arch" type="xml"><xpath expr="//form" position="inside"><field name="total"/></xpath></field></record><template id="card"><t t-call="clinic.other"/></template></odoo>`, "local", "his")
	odooTestExtract(t, g, "clinic/security/ir.model.access.csv", "id,name,model_id:id,group_id:id,perm_read,perm_write,perm_create,perm_unlink\naccess_patient,Patient,model_clinic_patient,base.group_user,1,0,0,0\n", "local", "his")
	odooTestExtract(t, g, "base/security/groups.xml", `<odoo><record id="group_user" model="res.groups"><field name="name">User</field></record></odoo>`, "core", "his")
	odooTestExtract(t, g, "clinic/static/src/widget.js", `/** @odoo-module **/
import { registry } from '@web/core/registry';
import { useService } from '@web/core/utils/hooks';
class Card { static template = 'clinic.card'; setup() { this.orm = useService('orm'); this.orm.call('clinic.patient', 'action_open', []); } }
registry.category('actions').add('clinic.card', Card);
registry.category('services').add('orm', Card);
`, "local", "his")
	selection, err := NewFrameworkSynthesizerSelection([]string{SynthOdoo})
	if err != nil {
		t.Fatal(err)
	}
	report := RunFrameworkSynthesizersWithSelection(g, selection)
	if report.Total == 0 {
		t.Fatal("registered pass did not run")
	}
	model := "clinic/models/patient.py::Patient"
	base := "base/models/partner.py::Partner"
	odooTestEdge(t, g, model, base, "inherit")
	odooTestEdge(t, g, model, base, "delegates")
	odooTestEdge(t, g, model+".partner_id", base, "relation")
	odooTestEdge(t, g, model+".total", model+"._compute_total", "compute")
	odooTestEdge(t, g, model+"._compute_total", base+".name", "api.depends")
	odooTestEdge(t, g, model+".partner_name", base+".name", "related")
	view := "clinic/views/patient.xml::odoo:patient_form"
	odooTestEdge(t, g, view, model+".partner_id", "view_field")
	odooTestEdge(t, g, view, model+".action_open", "object")
	odooTestEdge(t, g, "clinic/views/patient.xml::odoo:patient_inherit", view, "inherit_id")
	odooTestEdge(t, g, model+".action_open", view, "ref")
	odooTestEdge(t, g, "clinic/security/ir.model.access.csv::odoo:access_patient", model, "model_id:id")
	odooTestEdge(t, g, "clinic/security/ir.model.access.csv::odoo:access_patient", "base/security/groups.xml::odoo:group_user", "group_id:id")
	odooTestEdge(t, g, "clinic/__manifest__.py::odoo:clinic", "base/__manifest__.py::odoo:base", "depends_on")
	odooTestEdge(t, g, "clinic/static/src/widget.js::Card", "clinic/views/patient.xml::odoo:card", "template")
	first := ResolveOdoo(g)
	if again := ResolveOdoo(g); again != first {
		t.Fatalf("non-idempotent: %d then %d", first, again)
	}
	// A target rename must remove stale edges even if the source file did not change.
	n := g.GetNode(base)
	n.Meta["odoo_model"] = "res.renamed"
	g.AddNode(n)
	ResolveOdooScoped(g, map[string]bool{"core": true})
	for _, e := range g.GetOutEdges(model) {
		if e.To == base && e.Meta[MetaSynthesizedBy] == SynthOdoo {
			t.Fatal("stale binding after target rename")
		}
	}
	n.Meta["odoo_model"] = "res.partner"
	g.AddNode(n)
	ResolveOdooScoped(g, map[string]bool{"core": true})
	odooTestEdge(t, g, model, base, "inherit")
}

func TestOdooInheritedAndNestedViews(t *testing.T) {
	g := graph.New()
	odooTestExtract(t, g, "demo/__manifest__.py", `{'name':'Demo'}`, "repo", "ws")
	odooTestExtract(t, g, "demo/model.py", `from odoo import fields, models
class Parent(models.Model):
    _name = 'demo.parent'
    name = fields.Char()
    def action_open(self): pass
class Child(models.Model):
    _name = 'demo.child'
    _inherit = 'demo.parent'
    partner_id = fields.Many2one('demo.parent')
class Delegated(models.Model):
    _name = 'demo.delegated'
    _inherits = {'demo.parent': 'parent_id'}
    parent_id = fields.Many2one('demo.parent')
`, "repo", "ws")
	odooTestExtract(t, g, "demo/view.xml", `<odoo><record id="base" model="ir.ui.view"><field name="name">Name is view metadata</field><field name="model">demo.child</field><field name="arch" type="xml"><form><field name="partner_id"><form><field name="name"/><button name="action_open" type="object"/></form></field></form></field></record><record id="extension" model="ir.ui.view"><field name="inherit_id" ref="base"/><field name="arch" type="xml"><xpath expr="//form" position="inside"><field name="name"/></xpath></field></record><record id="delegated" model="ir.ui.view"><field name="model">demo.delegated</field><field name="arch" type="xml"><form><field name="name"/><button type="object" name="action_open"/></form></field></record></odoo>`, "repo", "ws")
	ResolveOdoo(g)
	odooTestEdge(t, g, "demo/view.xml::odoo:base", "demo/model.py::Parent.name", "view_field")
	odooTestEdge(t, g, "demo/view.xml::odoo:base", "demo/model.py::Parent.action_open", "object")
	odooTestEdge(t, g, "demo/view.xml::odoo:extension", "demo/model.py::Parent.name", "view_field")
	odooTestEdge(t, g, "demo/view.xml::odoo:delegated", "demo/model.py::Parent.name", "view_field")
	for _, e := range g.GetOutEdges("demo/view.xml::odoo:delegated") {
		if e.To == "demo/model.py::Parent.action_open" {
			t.Fatal("_inherits must not inherit methods")
		}
	}
}

func TestOdooWorkspaceAndAddonShadow(t *testing.T) {
	g := graph.New()
	for _, repo := range []string{"local", "copy", "other"} {
		workspace := "his"
		if repo == "other" {
			workspace = "other"
		}
		odooTestExtract(t, g, repo+"/clinic/__manifest__.py", `{'name':'Clinic'}`, repo, workspace)
		odooTestExtract(t, g, repo+"/clinic/model.py", `from odoo import models
class Patient(models.Model):
    _name = 'clinic.patient'
`, repo, workspace)
	}
	odooTestExtract(t, g, "local/clinic/view.xml", `<odoo><record id="view" model="ir.ui.view"><field name="model">clinic.patient</field></record></odoo>`, "local", "his")
	ResolveOdoo(g)
	from := "local/clinic/view.xml::odoo:view"
	odooTestEdge(t, g, from, "local/clinic/model.py::Patient", "model")
	for _, e := range g.GetOutEdges(from) {
		if strings.HasPrefix(e.To, "copy/") || strings.HasPrefix(e.To, "other/") {
			t.Fatalf("cross-copy/workspace binding: %+v", e)
		}
	}
}

func TestOdooManifestPrefixedFilesAndAssets(t *testing.T) {
	g := graph.New()
	odooTestExtract(t, g, "demo/__manifest__.py", `{'name':'Demo','data':['views.xml'],'assets':{'web.assets_backend':['demo/static/src/**/*.scss']}}`, "repo", "ws")
	odooTestExtract(t, g, "demo/views.xml", `<odoo/>`, "repo", "ws")
	// Match the indexer's post-extraction repository path rewriting. Facts
	// retain the spelling from source; file identities gain the repo prefix.
	for _, n := range g.AllNodes() {
		n.FilePath = "repo/" + n.FilePath
		g.AddNode(n)
	}
	asset := &graph.Node{ID: "repo/demo/static/src/nested/main.scss", Kind: graph.KindFile, FilePath: "repo/demo/static/src/nested/main.scss", Language: "scss", RepoPrefix: "repo", WorkspaceID: "ws"}
	g.AddNode(asset)
	ResolveOdoo(g)
	odooTestEdge(t, g, "demo/__manifest__.py::odoo:demo", "demo/views.xml", "data")
	odooTestEdge(t, g, "demo/__manifest__.py::odoo:asset:web.assets_backend", asset.ID, "asset")
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"demo/**/*.js", "demo/main.js", true}, {"demo/**/*.js", "demo/a/b/main.js", true},
		{"demo/*.js", "demo/a/main.js", false}, {"demo/**/x/*.js", "demo/x/main.css", false},
	} {
		if got := odooAssetMatch(tc.pattern, tc.name); got != tc.want {
			t.Errorf("glob %s %s = %v", tc.pattern, tc.name, got)
		}
	}
}

// Opt-in real-source smoke test; reads the user's Odoo checkout without
// starting Odoo, contacting services, modifying it, or touching the daemon.
func TestOdooHISCorpus(t *testing.T) {
	root := os.Getenv("GORTEX_ODOO_TEST_ROOT")
	if root == "" {
		t.Skip("set GORTEX_ODOO_TEST_ROOT to an Odoo addon checkout")
	}
	g := graph.New()
	for _, file := range []string{"his_dhp/__manifest__.py", "his_dhp/models/his_patient.py", "his_dhp/views/his_patient_views.xml", "his_dhp/security/ir.model.access.csv"} {
		b, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Fatal(err)
		}
		odooTestExtract(t, g, file, string(b), "local", "his")
	}
	if count := ResolveOdoo(g); count == 0 {
		t.Fatal("no real Odoo links")
	}
	odooTestEdge(t, g, "his_dhp/views/his_patient_views.xml::odoo:view_his_patient_dhp_inherit", "his_dhp/models/his_patient.py::HisPatientDhp.action_dhp_fetch_patient", "object")
	odooTestEdge(t, g, "his_dhp/views/his_patient_views.xml::odoo:view_his_patient_dhp_inherit", "his_dhp/models/his_patient.py::HisPatientDhp.dhp_fhir_id", "view_field")
}
