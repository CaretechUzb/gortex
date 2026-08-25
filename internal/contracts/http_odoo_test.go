package contracts

import (
	"strings"
	"testing"
)

func odooContracts(t *testing.T, src string) []Contract {
	t.Helper()
	h := &HTTPExtractor{}
	lines := strings.Split(src, "\n")
	return h.extractOdooRoutes("controllers/main.py", src, lines, nil, "python", nil)
}

func hasOdooRoute(cs []Contract, id string) bool {
	for _, c := range cs {
		if c.ID == id {
			return true
		}
	}
	return false
}

func odooRouteMeta(t *testing.T, cs []Contract, id string) map[string]any {
	t.Helper()
	for _, c := range cs {
		if c.ID == id {
			return c.Meta
		}
	}
	t.Fatalf("no contract %q in %v", id, odooContractIDs(cs))
	return nil
}

func odooContractIDs(cs []Contract) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

// Odoo's default when `methods=` is absent is any method — in practice
// GET and POST. Inheriting Flask's GET-only default would silently drop
// every form POST in an Odoo codebase.
func TestOdooRoutes_DefaultsToGetAndPost(t *testing.T) {
	cs := odooContracts(t, `
class Main(http.Controller):
    @http.route('/web/login', type='http', auth='public')
    def login(self, **kw):
        return None
`)
	if !hasOdooRoute(cs, "http::GET::/web/login") || !hasOdooRoute(cs, "http::POST::/web/login") {
		t.Errorf("expected GET+POST for a method-less route, got %v", odooContractIDs(cs))
	}
}

func TestOdooRoutes_ExplicitMethodsWin(t *testing.T) {
	cs := odooContracts(t, `
    @http.route('/api/ping', type='json', auth='none', methods=['POST'])
    def ping(self):
        return {}
`)
	if hasOdooRoute(cs, "http::GET::/api/ping") {
		t.Errorf("methods=['POST'] must not yield GET, got %v", odooContractIDs(cs))
	}
	if !hasOdooRoute(cs, "http::POST::/api/ping") {
		t.Errorf("expected POST, got %v", odooContractIDs(cs))
	}
	if got := odooRouteMeta(t, cs, "http::POST::/api/ping")["odoo_type"]; got != "json" {
		t.Errorf("odoo_type = %v, want json", got)
	}
	if got := odooRouteMeta(t, cs, "http::POST::/api/ping")["odoo_auth"]; got != "none" {
		t.Errorf("odoo_auth = %v, want none", got)
	}
}

// One decorator may declare several paths; a pass that reads only the
// first string literal silently loses the rest.
func TestOdooRoutes_PathList(t *testing.T) {
	cs := odooContracts(t, `
    @http.route(['/shop', '/shop/page/<int:page>'], type='http', auth='public', methods=['GET'])
    def shop(self, page=0, **kw):
        return None
`)
	if !hasOdooRoute(cs, "http::GET::/shop") {
		t.Errorf("first path missing, got %v", odooContractIDs(cs))
	}
	if !hasOdooRoute(cs, "http::GET::/shop/page/{p1}") {
		t.Errorf("second path missing or unnormalised, got %v", odooContractIDs(cs))
	}
}

// Odoo decorators wrap freely across lines; the argument text has to be
// accumulated by paren balance or both the path list and methods= are
// truncated.
func TestOdooRoutes_MultiLineDecorator(t *testing.T) {
	cs := odooContracts(t, `
    @http.route(
        ['/a', '/b'],
        type='http',
        auth='user',
        methods=['GET'],
    )
    def handler(self):
        return None
`)
	if !hasOdooRoute(cs, "http::GET::/a") || !hasOdooRoute(cs, "http::GET::/b") {
		t.Errorf("multi-line decorator lost a path, got %v", odooContractIDs(cs))
	}
	if hasOdooRoute(cs, "http::POST::/a") {
		t.Errorf("multi-line methods= must be honoured, got %v", odooContractIDs(cs))
	}
}

// <model("res.partner"):partner> defeats the shared paramPatterns regex,
// so it must be reduced to a plain slot before normalisation — and the
// model it named is worth keeping.
func TestOdooRoutes_ModelConverterIsNormalised(t *testing.T) {
	cs := odooContracts(t, `
    @http.route('/partner/<model("res.partner"):partner>/edit', type='http', auth='user', methods=['GET'])
    def edit(self, partner):
        return None
`)
	if !hasOdooRoute(cs, "http::GET::/partner/{p1}/edit") {
		t.Fatalf("model converter not normalised, got %v", odooContractIDs(cs))
	}
	meta := odooRouteMeta(t, cs, "http::GET::/partner/{p1}/edit")
	models, ok := meta["odoo_path_models"].(map[string]string)
	if !ok {
		t.Fatalf("odoo_path_models missing, meta=%v", meta)
	}
	if models["partner"] != "res.partner" {
		t.Errorf("odoo_path_models = %v, want partner→res.partner", models)
	}
	names, _ := meta["path_param_names"].([]string)
	if len(names) != 1 || names[0] != "partner" {
		t.Errorf("path_param_names = %v, want [partner]", names)
	}
}

// The bare @route(...) form is what `from odoo.http import route` gives.
func TestOdooRoutes_BareRouteDecorator(t *testing.T) {
	cs := odooContracts(t, `
    @route('/bare', type='http', auth='public', methods=['GET'])
    def bare(self):
        return None
`)
	if !hasOdooRoute(cs, "http::GET::/bare") {
		t.Errorf("bare @route form missed, got %v", odooContractIDs(cs))
	}
}

// A keyword value that happens to be quoted must never be read as a path.
func TestOdooRoutes_KeywordValuesAreNotPaths(t *testing.T) {
	cs := odooContracts(t, `
    @http.route('/only', type='http', auth='public', methods=['GET'])
    def only(self):
        return None
`)
	if len(cs) != 1 {
		t.Errorf("expected exactly one contract, got %v", odooContractIDs(cs))
	}
}

func TestOdooRoutes_BoolKwargs(t *testing.T) {
	cs := odooContracts(t, `
    @http.route('/site', type='http', auth='public', website=True, csrf=False, methods=['GET'])
    def site(self):
        return None
`)
	meta := odooRouteMeta(t, cs, "http::GET::/site")
	if meta["odoo_website"] != true {
		t.Errorf("odoo_website = %v, want true", meta["odoo_website"])
	}
	if meta["odoo_csrf"] != false {
		t.Errorf("odoo_csrf = %v, want false", meta["odoo_csrf"])
	}
}

func TestOdooRoutes_IgnoresNonRouteFile(t *testing.T) {
	if cs := odooContracts(t, "def plain():\n    return 1\n"); len(cs) != 0 {
		t.Errorf("expected no contracts, got %v", odooContractIDs(cs))
	}
}
