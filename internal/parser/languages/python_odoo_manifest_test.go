package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func odooManifest(t *testing.T, filePath, src string) *parser.ExtractionResult {
	t.Helper()
	res, err := NewPythonExtractor().Extract(filePath, []byte(src))
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	return res
}

const saleManifest = `
{
    "name": "Sales",
    "version": "17.0.1.0.0",
    "category": "Sales/Sales",
    "license": "LGPL-3",
    "application": True,
    "depends": ["base", "account", "portal"],
    "data": [
        "security/ir.model.access.csv",
        "views/sale_views.xml",
    ],
    "demo": ["demo/sale_demo.xml"],
    "assets": {
        "web.assets_backend": [
            "sale/static/src/js/sale.js",
            "sale/static/src/**/*.scss",
        ],
    },
}
`

// The module's identity is its directory name; the manifest's "name" key
// is a human label and must not be mistaken for an identifier.
func TestOdooManifest_ModuleNodeUsesDirectoryName(t *testing.T) {
	res := odooManifest(t, "addons/sale/__manifest__.py", saleManifest)

	n := odooNode(res, odooModuleNodeID("sale", "17.0.1.0.0"))
	if n == nil {
		t.Fatalf("no module node; got %v", odooNodeIDs(res))
	}
	if n.Kind != graph.KindModule {
		t.Errorf("kind = %v, want module", n.Kind)
	}
	if n.Name != "sale" {
		t.Errorf("Name = %q, want the directory name sale", n.Name)
	}
	if got := n.Meta["odoo_display_name"]; got != "Sales" {
		t.Errorf("odoo_display_name = %v, want Sales", got)
	}
	if got := n.Meta["odoo_application"]; got != true {
		t.Errorf("odoo_application = %v, want true", got)
	}
	if got := n.Meta["odoo_license"]; got != "LGPL-3" {
		t.Errorf("odoo_license = %v", got)
	}
}

func TestOdooManifest_DependsBecomeModuleEdges(t *testing.T) {
	res := odooManifest(t, "addons/sale/__manifest__.py", saleManifest)
	for _, dep := range []string{"base", "account", "portal"} {
		if e := odooHasEdge(res, graph.EdgeDependsOnModule, odooModuleNodeID(dep, "")); e == nil {
			t.Errorf("no depends_on_module edge for %q", dep)
		}
	}
}

// The manifest is the only authority on which data files belong to which
// module, and the paths resolve against the manifest's own directory.
func TestOdooManifest_DataFilesResolveToRealPaths(t *testing.T) {
	res := odooManifest(t, "addons/sale/__manifest__.py", saleManifest)

	for _, want := range []string{
		"addons/sale/security/ir.model.access.csv",
		"addons/sale/views/sale_views.xml",
	} {
		e := odooHasEdge(res, graph.EdgeReferences, want)
		if e == nil {
			t.Fatalf("no data edge to %q", want)
		}
		if e.Meta["odoo_link"] != "data" {
			t.Errorf("edge to %q: odoo_link = %v, want data", want, e.Meta["odoo_link"])
		}
	}
	e := odooHasEdge(res, graph.EdgeReferences, "addons/sale/demo/sale_demo.xml")
	if e == nil || e.Meta["odoo_link"] != "demo" {
		t.Errorf("demo file edge missing or mislabelled: %+v", e)
	}
}

// Asset paths are module-relative with the module name as the first
// segment, so that segment must be dropped rather than doubled.
func TestOdooManifest_AssetPathsStripModulePrefix(t *testing.T) {
	res := odooManifest(t, "addons/sale/__manifest__.py", saleManifest)

	e := odooHasEdge(res, graph.EdgeReferences, "addons/sale/static/src/js/sale.js")
	if e == nil {
		t.Fatalf("no asset edge; got edges %v", odooEdgeTargets(res))
	}
	if got := e.Meta["odoo_bundle"]; got != "web.assets_backend" {
		t.Errorf("odoo_bundle = %v", got)
	}
	// A glob names a set, not a file — no edge to a non-existent path.
	for _, to := range odooEdgeTargets(res) {
		if to == "addons/sale/static/src/**/*.scss" {
			t.Error("a glob asset entry must not mint an edge")
		}
	}
}

// A .py file that is not a manifest must be untouched by this pass.
func TestOdooManifest_IgnoresOrdinaryPythonFile(t *testing.T) {
	res := odooManifest(t, "addons/sale/models/sale_order.py", `{"name": "not a manifest"}`)
	for _, n := range res.Nodes {
		if n != nil && n.Kind == graph.KindModule {
			t.Errorf("ordinary .py must not yield a module node: %s", n.ID)
		}
	}
}

// Pre-v10 addons still use __openerp__.py.
func TestOdooManifest_LegacyOpenerpBasename(t *testing.T) {
	res := odooManifest(t, "addons/legacy/__openerp__.py", `{"name": "Legacy", "depends": ["base"]}`)
	if odooNode(res, odooModuleNodeID("legacy", "")) == nil {
		t.Errorf("__openerp__.py must be recognised; got %v", odooNodeIDs(res))
	}
}

func odooNodeIDs(res *parser.ExtractionResult) []string {
	out := make([]string, 0, len(res.Nodes))
	for _, n := range res.Nodes {
		if n != nil {
			out = append(out, n.ID)
		}
	}
	return out
}

func odooEdgeTargets(res *parser.ExtractionResult) []string {
	out := make([]string, 0, len(res.Edges))
	for _, e := range res.Edges {
		if e != nil {
			out = append(out, e.To)
		}
	}
	return out
}
