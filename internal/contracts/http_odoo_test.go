package contracts

import (
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/frameworkgate"
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

// The converter takes an optional second argument — a domain restricting
// the records the slot matches — and that domain carries commas, quotes
// and parens of its own. Requiring `)` straight after the model name left
// this form unrewritten, which is exactly the unmatchable-ID failure the
// converter rewrite exists to prevent.
func TestOdooRoutes_ModelConverterWithDomain(t *testing.T) {
	cs := odooContracts(t, `
    @http.route('''/event/<model("event.event", "[('website_track', '=', True)]"):event>/track''', type='http', auth='public', methods=['GET'])
    def track(self, event):
        return None
`)
	if !hasOdooRoute(cs, "http::GET::/event/{p1}/track") {
		t.Fatalf("domain-carrying converter not normalised, got %v", odooContractIDs(cs))
	}
	models, ok := odooRouteMeta(t, cs, "http::GET::/event/{p1}/track")["odoo_path_models"].(map[string]string)
	if !ok || models["event"] != "event.event" {
		t.Errorf("odoo_path_models = %v, want event→event.event", models)
	}
}

// A domain compares, and `>` / `>=` are ordinary Odoo operators. Skipping
// the domain with `[^>]*` halted on the operator, never reached the
// converter's closing paren, and failed the match outright — so the path
// kept its raw converter text and the slot → model link was lost too.
func TestOdooRoutes_ModelConverterWithComparisonDomain(t *testing.T) {
	for _, domain := range []string{
		`"[('date_end', '>=', now)]"`,
		`"[('seats_available', '>', 0)]"`,
		`'[("date_end", ">=", now)]'`,
	} {
		cs := odooContracts(t, `
    @http.route('''/event/<model("event.event", `+domain+`):event>/register''', type='http', auth='public', methods=['GET'])
    def register(self, event):
        return None
`)
		if !hasOdooRoute(cs, "http::GET::/event/{p1}/register") {
			t.Fatalf("domain %s left the converter unrewritten, got %v", domain, odooContractIDs(cs))
		}
		models, ok := odooRouteMeta(t, cs, "http::GET::/event/{p1}/register")["odoo_path_models"].(map[string]string)
		if !ok || models["event"] != "event.event" {
			t.Errorf("domain %s: odoo_path_models = %v, want event→event.event", domain, models)
		}
	}
}

// Two converters in one path must stay two matches: a pattern that let the
// first one run to the last `)` in the path would swallow both and report
// the first model against the second slot.
func TestOdooRoutes_TwoModelConvertersInOnePath(t *testing.T) {
	cs := odooContracts(t, `
    @http.route('/forum/<model("forum.forum"):forum>/post/<model("forum.post"):post>', type='http', auth='public', methods=['GET'])
    def post(self, forum, post):
        return None
`)
	if !hasOdooRoute(cs, "http::GET::/forum/{p1}/post/{p2}") {
		t.Fatalf("two converters not both normalised, got %v", odooContractIDs(cs))
	}
	models, _ := odooRouteMeta(t, cs, "http::GET::/forum/{p1}/post/{p2}")["odoo_path_models"].(map[string]string)
	if models["forum"] != "forum.forum" || models["post"] != "forum.post" {
		t.Errorf("odoo_path_models = %v, want forum→forum.forum and post→forum.post", models)
	}
}

// The plural converter is an ordinary sibling of the singular one and
// takes a SINGLE colon, not the `::` an earlier comment claimed.
func TestOdooRoutes_PluralModelConverter(t *testing.T) {
	cs := odooContracts(t, `
    @http.route('/files/<models("ir.attachment"):attachments>', type='http', auth='user', methods=['GET'])
    def files(self, attachments):
        return None
`)
	if !hasOdooRoute(cs, "http::GET::/files/{p1}") {
		t.Fatalf("plural converter not normalised, got %v", odooContractIDs(cs))
	}
	models, _ := odooRouteMeta(t, cs, "http::GET::/files/{p1}")["odoo_path_models"].(map[string]string)
	if models["attachments"] != "ir.attachment" {
		t.Errorf("odoo_path_models = %v, want attachments→ir.attachment", models)
	}
}

// A werkzeug converter argument is itself spelled `name=value`, so a
// kwarg scan that does not span quoted regions reads `min=` as the start
// of the decorator's keyword arguments, truncates the head mid-path, and
// drops the whole route — path and methods= alike.
func TestOdooRoutes_ConverterArgumentIsNotAKwarg(t *testing.T) {
	cs := odooContracts(t, `
    @http.route('/page/<int(min=1):page>', type='http', auth='public', methods=['GET'])
    def page(self, page=1):
        return None
`)
	if !hasOdooRoute(cs, "http::GET::/page/{p1}") {
		t.Fatalf("converter argument dropped or mangled the route, got %v", odooContractIDs(cs))
	}
	if hasOdooRoute(cs, "http::POST::/page/{p1}") {
		t.Errorf("methods= was lost with the truncated head, got %v", odooContractIDs(cs))
	}
}

// Same shape inside the list form, and with a converter argument that is
// itself quoted.
func TestOdooRoutes_ConverterArgumentsInPathList(t *testing.T) {
	cs := odooContracts(t, `
    @http.route(['/l/<string(length=2):lang>', '/p/<int(min=1,max=9):n>'], type='http', auth='public', methods=['GET'])
    def multi(self, lang=None, n=1):
        return None
`)
	if !hasOdooRoute(cs, "http::GET::/l/{p1}") || !hasOdooRoute(cs, "http::GET::/p/{p1}") {
		t.Fatalf("converter arguments broke the path list, got %v", odooContractIDs(cs))
	}
}

// The domain form is written triple-quoted in real Odoo precisely because
// it needs both quote styles; a scanner that closes on the first matching
// single quote truncates the path mid-converter.
func TestOdooStringLiterals_TripleQuoted(t *testing.T) {
	got := odooStringLiterals(`'''/event/<model("event.event", "[('website_track', '=', True)]"):event>'''`)
	want := `/event/<model("event.event", "[('website_track', '=', True)]"):event>`
	if len(got) != 1 || got[0] != want {
		t.Errorf("odooStringLiterals = %q, want [%q]", got, want)
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

// `@http.route(...)` is claimed by BOTH the flask-decorator pass (whose
// regex is `@(\w+)\.route\(`) and the odoo pass. The registry unions its
// passes with no dedup, so before the flask pass yielded on an `http`
// receiver the single-string form minted two contracts under the same ID
// with conflicting frameworks — and whichever landed last won.
func TestOdooRoutes_SingleStringFormIsNotAlsoClaimedByFlask(t *testing.T) {
	src := `
class Main(http.Controller):
    @http.route('/shop', type='http', auth='public')
    def shop(self):
        return None
`
	ctx := &RouteExtractCtx{
		FilePath: "controllers/main.py", Src: []byte(src), Text: src,
		Lines: strings.Split(src, "\n"), Lang: "python", H: &HTTPExtractor{},
	}
	cs := runFrameworkRoutePasses(ctx)
	seen := map[string]int{}
	for _, c := range cs {
		seen[c.ID]++
		if c.Meta["framework"] == "flask" {
			t.Errorf("%s attributed to flask, want odoo: %v", c.ID, c.Meta)
		}
	}
	if seen["http::GET::/shop"] != 1 {
		t.Errorf("http::GET::/shop emitted %d times, want 1: %v", seen["http::GET::/shop"], odooContractIDs(cs))
	}
	// Odoo's method-less default must survive the hand-over.
	if seen["http::POST::/shop"] != 1 {
		t.Errorf("odoo POST default lost, got %v", odooContractIDs(cs))
	}
}

// Excluding `odoo` must hand the route back to flask rather than drop it:
// the yield above is gated on the odoo pass actually being allowed to run.
func TestFlaskDecorator_KeepsHTTPReceiverWhenOdooExcluded(t *testing.T) {
	src := `
    @http.route('/shop', methods=['GET'])
    def shop():
        return None
`
	ctx := &RouteExtractCtx{
		FilePath: "app.py", Src: []byte(src), Text: src,
		Lines: strings.Split(src, "\n"), Lang: "python",
		H: &HTTPExtractor{AllowedFrameworks: frameworkgate.New([]string{"flask-decorator"})},
	}
	cs := runFrameworkRoutePasses(ctx)
	if !hasOdooRoute(cs, "http::GET::/shop") {
		t.Fatalf("route dropped when odoo pass is excluded, got %v", odooContractIDs(cs))
	}
	if got := odooRouteMeta(t, cs, "http::GET::/shop")["framework"]; got != "flask" {
		t.Errorf("framework = %v, want flask", got)
	}
}

func TestOdooRoutes_IgnoresNonRouteFile(t *testing.T) {
	if cs := odooContracts(t, "def plain():\n    return 1\n"); len(cs) != 0 {
		t.Errorf("expected no contracts, got %v", odooContractIDs(cs))
	}
}
