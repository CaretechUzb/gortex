package contracts

import (
	"testing"

	"github.com/zzet/gortex/internal/parser/languages"
)

func TestOdooHTTPContracts(t *testing.T) {
	src := []byte(`from odoo import http
class Controller(http.Controller):
    @http.route('/demo', auth='user', methods=['GET', 'POST'], csrf=False)
    def plain(self): pass
    @http.route(['/demo/<model("res.partner"):partner>', '/alias'], type='http')
    def converted(self, partner=None): pass
`)
	r, err := languages.NewPythonExtractor().Extract("controller.py", src)
	if err != nil {
		t.Fatal(err)
	}
	contracts := (&HTTPExtractor{}).Extract("controller.py", src, r.Nodes, nil)
	found := map[string]bool{}
	for _, c := range contracts {
		if c.Role != RoleProvider {
			continue
		}
		if c.Meta["framework"] != "odoo" {
			t.Fatalf("Odoo route mislabeled: %+v", c)
		}
		if c.SymbolID == "" {
			t.Fatal("route has no handler")
		}
		if found[c.ID] {
			t.Fatalf("duplicate route: %s", c.ID)
		}
		found[c.ID] = true
		if c.Meta["odoo_options"] == nil {
			t.Fatal("route security options lost")
		}
	}
	for _, id := range []string{"http::GET::/demo", "http::POST::/demo", "http::ANY::/demo/{p1}", "http::ANY::/alias"} {
		if !found[id] {
			t.Errorf("missing %s; got %v", id, found)
		}
	}
}
