package main

// This suite is hand-authored, not a snapshot of extractor output. Each query
// labels its complete result set. Keep unsupported/unassessed areas visible in
// the inventory instead of interpreting a green result as universal coverage.
func builtinSuite() suite {
	s := suite{Version: "odoo-static-v2"}
	for _, f := range []feature{
		{ID: "models", Description: "Model names, extension bases and specifications"},
		{ID: "model-inheritance", Description: "Classical and in-place model inheritance"},
		{ID: "delegation", Description: "Delegated fields without delegated methods"},
		{ID: "fields", Description: "Field types, comodels and related chains"},
		{ID: "callbacks", Description: "Compute, inverse and search callbacks"},
		{ID: "api-decorators", Description: "Depends, constrains and onchange field bindings"},
		{ID: "environment-lookups", Description: "Literal model and external-ID references"},
		{ID: "manifest", Description: "Manifest specifications, dependencies and data files"},
		{ID: "hooks", Description: "Addon-local installation hooks"},
		{ID: "assets", Description: "Literal assets and recursive globs"},
		{ID: "xml-records", Description: "Arbitrary record models and values"},
		{ID: "view-inheritance", Description: "Inherited view model and XPath specifications"},
		{ID: "view-bindings", Description: "Fields, object buttons and nested relational subviews"},
		{ID: "qweb", Description: "QWeb template calls and inheritance"},
		{ID: "security", Description: "ACL model/group IDs, permissions and rule data"},
		{ID: "actions-menus", Description: "Action records and menu references"},
		{ID: "reports", Description: "Report action specifications and templates"},
		{ID: "automation", Description: "Cron/server-action and mail template specifications"},
		{ID: "js-registry", Description: "Frontend registration and lookup keys"},
		{ID: "js-services", Description: "Literal service consumption"},
		{ID: "js-templates", Description: "Component template references"},
		{ID: "js-rpc", Description: "Literal ORM calls to model methods"},
		{ID: "js-patches", Description: "Patch source specifications"},
		{ID: "controllers", Description: "HTTP methods, path converters and handler ownership"},
		{ID: "isolation", Description: "Same-addon shadowing and workspace boundaries"},
		{ID: "detection", Description: "Odoo admission and rejection of unrelated sources"},
		{ID: "direct-field-imports", Description: "Unassessed: from odoo.fields import Char"},
		{ID: "api-returns", Description: "Unassessed: api.returns model relationships"},
		{ID: "legacy-versions", Description: "Unassessed: old ORM and legacy web API variants"},
		{ID: "asset-directives", Description: "Unassessed: tuple directive ordering and bundle includes"},
		{ID: "cross-file-js-patches", Description: "Unassessed: imported patch targets"},
		{ID: "incremental-rebinding", Description: "Unassessed here: changed-target scoped reconciliation (resolver unit tests cover it)"},
		{ID: "runtime-state", Description: "Installed modules, database/Studio declarations and access-right evaluation", RuntimeOnly: true},
		{ID: "dynamic-code", Description: "Computed names, executed server actions and arbitrary runtime registration", RuntimeOnly: true},
	} {
		s.Features = append(s.Features, f)
	}
	add := func(path, src string) {
		s.Files = append(s.Files, sourceFile{Path: path, Source: src, Repo: "addons", Workspace: "golden"})
	}
	add("base/__manifest__.py", `{'name': 'Base'}`)
	add("base/models.py", `from odoo import models, fields
class Partner(models.Model):
    _name = 'res.partner'
    name = fields.Char()
    def open(self): pass
`)
	add("base/groups.xml", `<odoo><record id="group_user" model="res.groups"><field name="name">User</field></record></odoo>`)
	add("demo/__manifest__.py", `{'name': 'Demo', 'depends': ['base'], 'data': ['views.xml', 'security/ir.model.access.csv'], 'post_init_hook': 'setup_hook', 'license': 'LGPL-3', 'assets': {'web.assets_backend': ['demo/static/src/**/*.js']}}`)
	add("demo/__init__.py", `def setup_hook(cr, registry): pass
`)
	add("demo/models.py", `from odoo import api, fields as f, models
class Item(models.Model):
    _name = 'demo.item'
    _inherit = 'res.partner'
    _description = 'Item'
    _order = 'name'
    _sql_constraints = [('name_unique', 'unique(name)', 'Unique name')]
    partner_id = f.Many2one('res.partner', required=True)
    total = f.Float(compute='_compute', inverse='_inverse', search='_search', store=True)
    partner_name = f.Char(related='partner_id.name')
    @api.depends('partner_id.name')
    def _compute(self): return self.env['res.partner'].search([])
    def _inverse(self): pass
    def _search(self, op, value): pass
    @api.constrains('total')
    def _check(self): pass
    @api.onchange('partner_id')
    def _onchange(self): pass
    def open(self): return self.env.ref('demo.form')
class Extension(models.Model):
    _inherit = 'demo.item'
    note = f.Text()
class Delegate(models.Model):
    _name = 'demo.delegate'
    _inherits = {'res.partner': 'partner_id'}
    partner_id = f.Many2one('res.partner')
class Wizard(models.TransientModel):
    _name = 'demo.wizard'
class Mixin(models.AbstractModel):
    _name = 'demo.mixin'
`)
	add("demo/views.xml", `<odoo><data noupdate="1">
<record id="form" model="ir.ui.view"><field name="name">Item</field><field name="model">demo.item</field><field name="arch" type="xml"><form><field name="total"/><field name="partner_id"><form><field name="name"/><button name="open" type="object"/></form></field><button name="open" type="object"/></form></field></record>
<record id="extension" model="ir.ui.view"><field name="inherit_id" ref="form"/><field name="arch" type="xml"><xpath expr="//form" position="inside"><field name="note"/></xpath></field></record>
<record id="delegated" model="ir.ui.view"><field name="model">demo.delegate</field><field name="arch" type="xml"><form><field name="name"/><button type="object" name="open"/></form></field></record>
<record id="action" model="ir.actions.act_window"><field name="res_model">demo.item</field><field name="view_id" ref="form"/></record>
<menuitem id="menu" name="Items" action="action" groups="base.group_user"/>
<record id="rule" model="ir.rule"><field name="model_id" ref="model_demo_item"/><field name="domain_force">[('active', '=', True)]</field></record>
<record id="report" model="ir.actions.report"><field name="model">demo.item</field><field name="report_name">demo.card</field></record>
<record id="cron" model="ir.cron"><field name="model_id" ref="model_demo_item"/><field name="state">code</field><field name="code">model.open()</field></record>
<record id="mail" model="mail.template"><field name="model_id" ref="model_demo_item"/><field name="subject">Hello</field></record>
<record id="custom" model="x.custom"><field name="payload" eval="{'key': 7}"/></record>
<template id="card"><t t-call="demo.other"/></template><template id="other"><div>Other</div></template>
</data></odoo>`)
	add("demo/templates.xml", `<templates><t t-name="demo.Widget"><span>Widget</span></t><t t-name="demo.Extended" t-inherit="demo.Widget" t-inherit-mode="extension"><xpath expr="//span" position="inside"><b>Hello</b></xpath></t></templates>`)
	add("demo/security/ir.model.access.csv", "id,name,model_id:id,group_id:id,perm_read,perm_write,perm_create,perm_unlink\naccess_item,Item,model_demo_item,base.group_user,1,0,0,0\n")
	add("demo/static/src/widget.js", `/** @odoo-module **/
import { registry } from '@web/core/registry';
import { useService } from '@web/core/utils/hooks';
class Widget {
    static template = 'demo.Widget';
    setup() {
        this.orm = useService('orm');
        this.orm.call('demo.item', 'open', []);
    }
}
registry.category('services').add('orm', Widget);
const actions = registry.category('actions');
actions.add('demo.widget', Widget);
actions.get('demo.widget');
patch(Widget.prototype, {patched: true});
`)
	add("demo/controllers.py", `from odoo import http
class Controller(http.Controller):
    @http.route('/items', methods=['GET', 'POST'], auth='user', csrf=False)
    def items(self): pass
    @http.route(['/items/<model("demo.item"):item>', '/alias'], auth='public')
    def detail(self, item=None): pass
`)
	add("plain/models.py", `class NotOdoo:
    _name = 'demo.fake'
    def query(self): return env['demo.item']
`)
	add("plain/data.xml", `<settings><record id="fake" model="demo.item"/></settings>`)
	add("plain/data.csv", "id,name\n1,Not Odoo\n")
	add("demo/dynamic.py", `from odoo import models
class Dynamic(models.Model):
    _name = 'demo.dynamic'
    def lookup(self, model, xmlid):
        return self.env[model], self.env.ref(xmlid)
`)
	// Complete node projections. Values below come from fixture semantics, not
	// the current index. JSON canonicalization only normalizes serialization.
	node := func(feature, name, subject, key string, value any) {
		// XML field metadata always carries all four schema columns, including
		// empty eval/ref attributes. Normalize only this storage representation.
		if key == "odoo_values" {
			for _, row := range value.([]map[string]string) {
				for _, column := range []string{"name", "value", "eval", "ref"} {
					if _, ok := row[column]; !ok {
						row[column] = ""
					}
				}
			}
		}
		s.Checks = append(s.Checks, check{Name: name, Feature: feature, Kind: "node", Subject: subject, Predicate: key, Expected: []string{subject + " = " + canonical(value)}})
	}
	edge := func(feature, name, subject, relation string, targets ...string) {
		c := check{Name: name, Feature: feature, Kind: "edge", Subject: subject, Predicate: relation, Expected: []string{}}
		for _, target := range targets {
			c.Expected = append(c.Expected, subject+" -> "+target)
		}
		s.Checks = append(s.Checks, c)
	}
	item := "demo/models.py::Item"
	partner := "base/models.py::Partner"
	view := "demo/views.xml::odoo:form"
	module := "demo/__manifest__.py::odoo:demo"
	node("models", "model name", item, "odoo_model", "demo.item")
	node("models", "model options", item, "odoo_spec", map[string]string{"_name": "'demo.item'", "_inherit": "'res.partner'", "_description": "'Item'", "_order": "'name'", "_sql_constraints": "[('name_unique', 'unique(name)', 'Unique name')]"})
	node("models", "transient model", "demo/models.py::Wizard", "odoo_bases", "(models.TransientModel)")
	node("models", "abstract model", "demo/models.py::Mixin", "odoo_bases", "(models.AbstractModel)")
	edge("model-inheritance", "classical inheritance", item, "inherit", partner)
	edge("model-inheritance", "extension inheritance", "demo/models.py::Extension", "inherit", item)
	edge("delegation", "composition", "demo/models.py::Delegate", "delegates", partner)
	edge("delegation", "delegated field", "demo/views.xml::odoo:delegated", "view_field", partner+".name")
	edge("delegation", "no delegated method", "demo/views.xml::odoo:delegated", "object")
	node("fields", "many2one type", item+".partner_id", "odoo_field_type", "Many2one")
	node("fields", "field options", item+".total", "odoo_spec", "f.Float(compute='_compute', inverse='_inverse', search='_search', store=True)")
	edge("fields", "comodel", item+".partner_id", "relation", partner)
	edge("fields", "related chain", item+".partner_name", "related", item+".partner_id", partner+".name")
	for _, cb := range []string{"compute", "inverse", "search"} {
		edge("callbacks", cb, item+".total", cb, item+"._"+cb)
	}
	edge("api-decorators", "depends path", item+"._compute", "api.depends", item+".partner_id", partner+".name")
	edge("api-decorators", "constraint dependency", item+"._check", "api.constrains", item+".total")
	edge("api-decorators", "onchange dependency", item+"._onchange", "api.onchange", item+".partner_id")
	edge("environment-lookups", "env model", item+"._compute", "env", partner)
	edge("environment-lookups", "env ref", item+".open", "ref", view)
	edge("environment-lookups", "dynamic lookups stay unresolved", "demo/dynamic.py::Dynamic.lookup", "")
	node("manifest", "manifest values", module, "odoo_spec", map[string]string{"name": "'Demo'", "depends": "['base']", "data": "['views.xml', 'security/ir.model.access.csv']", "post_init_hook": "'setup_hook'", "license": "'LGPL-3'", "assets": "{'web.assets_backend': ['demo/static/src/**/*.js']}"})
	edge("manifest", "addon dependency", module, "depends_on", "base/__manifest__.py::odoo:base")
	edge("manifest", "data files", module, "data", "demo/views.xml", "demo/security/ir.model.access.csv")
	edge("hooks", "post init hook", module, "post_init_hook", "demo/__init__.py::setup_hook")
	edge("assets", "recursive assets", "demo/__manifest__.py::odoo:asset:web.assets_backend", "asset", "demo/static/src/widget.js")
	node("xml-records", "arbitrary model", "demo/views.xml::odoo:custom", "odoo_record_model", "x.custom")
	node("xml-records", "eval payload", "demo/views.xml::odoo:custom", "odoo_values", []map[string]string{{"name": "payload", "eval": "{'key': 7}", "value": ""}})
	node("xml-records", "noupdate", view, "odoo_noupdate", "1")
	node("view-inheritance", "XPath specification", "demo/views.xml::odoo:extension", "odoo_xpath", []map[string]string{{"expr": "//form", "position": "inside"}})
	node("controllers", "route options", "demo/controllers.py::odoo:route:3:/items", "odoo_options", map[string]string{"methods": "['GET', 'POST']", "auth": "'user'", "csrf": "False"})
	edge("view-inheritance", "view parent", "demo/views.xml::odoo:extension", "inherit_id", view)
	edge("view-inheritance", "inferred inherited view model", "demo/views.xml::odoo:extension", "view_field", "demo/models.py::Extension.note")
	edge("view-bindings", "relational subview fields", view, "view_field", item+".total", item+".partner_id", partner+".name")
	edge("view-bindings", "relational subview buttons", view, "object", item+".open", partner+".open")
	edge("qweb", "template call", "demo/views.xml::odoo:card", "t-call", "demo/views.xml::odoo:other")
	edge("qweb", "template inheritance", "demo/templates.xml::odoo:demo.Extended", "t-inherit", "demo/templates.xml::odoo:demo.Widget")
	acl := "demo/security/ir.model.access.csv::odoo:access_item"
	edge("security", "ACL implicit model ID", acl, "model_id:id", item)
	edge("security", "ACL group", acl, "group_id:id", "base/groups.xml::odoo:group_user")
	node("security", "ACL permissions", acl, "odoo_spec", map[string]string{"id": "access_item", "name": "Item", "model_id:id": "model_demo_item", "group_id:id": "base.group_user", "perm_read": "1", "perm_write": "0", "perm_create": "0", "perm_unlink": "0"})
	node("security", "record rule domain", "demo/views.xml::odoo:rule", "odoo_values", []map[string]string{{"name": "model_id", "ref": "model_demo_item", "value": ""}, {"name": "domain_force", "value": "[('active', '=', True)]"}})
	edge("actions-menus", "menu action", "demo/views.xml::odoo:menu", "action", "demo/views.xml::odoo:action")
	node("actions-menus", "window action", "demo/views.xml::odoo:action", "odoo_record_model", "ir.actions.act_window")
	node("reports", "report action", "demo/views.xml::odoo:report", "odoo_record_model", "ir.actions.report")
	edge("reports", "report template", "demo/views.xml::odoo:report", "report_name", "demo/views.xml::odoo:card")
	node("automation", "cron code", "demo/views.xml::odoo:cron", "odoo_values", []map[string]string{{"name": "model_id", "ref": "model_demo_item", "value": ""}, {"name": "state", "value": "code"}, {"name": "code", "value": "model.open()"}})
	node("automation", "mail template", "demo/views.xml::odoo:mail", "odoo_record_model", "mail.template")
	js := "demo/static/src/widget.js"
	node("js-registry", "registry entry", js+"::odoo:registry:actions/demo.widget", "odoo_registry", "actions/demo.widget")
	edge("js-registry", "registry lookup", js, "get", js+"::odoo:registry:actions/demo.widget")
	edge("js-services", "service consumption", js+"::Widget.setup", "service", js+"::odoo:registry:services/orm")
	edge("js-templates", "component template", js+"::Widget", "template", "demo/templates.xml::odoo:demo.Widget")
	edge("js-rpc", "ORM method", js+"::Widget.setup", "rpc", item+".open")
	node("js-patches", "patch specification", js+"::odoo:patch:15", "odoo_spec", "patch(Widget.prototype, {patched: true})")
	s.Checks = append(s.Checks, check{Name: "controller routes", Feature: "controllers", Kind: "contract", File: "demo/controllers.py", Expected: []string{"demo/controllers.py::Controller.items -> http::GET::/items [odoo]", "demo/controllers.py::Controller.items -> http::POST::/items [odoo]", "demo/controllers.py::Controller.detail -> http::ANY::/items/{p1} [odoo]", "demo/controllers.py::Controller.detail -> http::ANY::/alias [odoo]"}})
	for _, path := range []string{"plain/models.py", "plain/data.xml", "plain/data.csv"} {
		s.Checks = append(s.Checks, check{Name: "reject " + path, Feature: "detection", Kind: "node", File: path, Predicate: "framework", Expected: []string{}})
	}
	node("detection", "accept Odoo XML", view, "framework", "odoo")
	// Same named addons in two repositories, and one separate workspace. Query
	// all model links from the view: any leaked copy becomes a false positive.
	for _, copy := range []struct{ repo, ws string }{{"local", "golden"}, {"copy", "golden"}, {"other", "other"}} {
		s.Files = append(s.Files, sourceFile{Path: copy.repo + "/isolated/__manifest__.py", Source: `{'name': 'Isolated'}`, Repo: copy.repo, Workspace: copy.ws}, sourceFile{Path: copy.repo + "/isolated/models.py", Source: "from odoo import models\nclass Isolated(models.Model):\n    _name = 'isolated.model'\n", Repo: copy.repo, Workspace: copy.ws})
	}
	s.Files = append(s.Files, sourceFile{Path: "local/isolated/view.xml", Source: `<odoo><record id="form" model="ir.ui.view"><field name="model">isolated.model</field></record></odoo>`, Repo: "local", Workspace: "golden"})
	edge("isolation", "copy and workspace isolation", "local/isolated/view.xml::odoo:form", "model", "local/isolated/models.py::Isolated")
	// Recovery cases are hand-labeled separately from ordinary ACL rows.
	s.Features = append(s.Features, feature{ID: "csv-recovery", Description: "Ragged rows, bare quotes, diagnostics and retained references"})
	csvFile := "demo/recovery/ir.model.access.csv"
	add(csvFile, "id,name,model_id:id,group_id:id\nshort,Short,model_demo_item\nextra,Extra,model_demo_item,base.group_user,unmapped.value\nbare\",Bare,model_demo_item,base.group_user\nlast,Last,model_demo_item,base.group_user\n,,,\n")
	short := csvFile + "::odoo:short"
	extra := csvFile + "::odoo:extra"
	bare := csvFile + "::odoo:bare\""
	node("csv-recovery", "missing column retained", short, "odoo_csv_missing", []string{"group_id:id"})
	node("csv-recovery", "extra column retained", extra, "odoo_csv_extra", []string{"unmapped.value"})
	node("csv-recovery", "bare quote diagnostic", bare, "odoo_csv_diagnostics", []string{"nonstandard_quotes"})
	node("csv-recovery", "bare quote ID unchanged", bare, "odoo_xmlid", "bare\"")
	edge("csv-recovery", "short row model reference", short, "model_id:id", item)
	edge("csv-recovery", "missing group not invented", short, "group_id:id")
	edge("csv-recovery", "row after bare quote retained", csvFile+"::odoo:last", "group_id:id", "base/groups.xml::odoo:group_user")
	return s
}
