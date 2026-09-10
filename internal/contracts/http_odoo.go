package contracts

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

var odooPathConverter = regexp.MustCompile(`<[^<>]*:([A-Za-z_][A-Za-z_0-9]*)>`)

func init() {
	RegisterFrameworkRoutePass(&routePass{
		name: "odoo", langs: []string{"python"},
		detect: func(_ string, src []byte) bool { return bytes.Contains(src, []byte("route")) },
		run: func(_ *HTTPExtractor, ctx *RouteExtractCtx) []Contract {
			var out []Contract
			for _, n := range ctx.FileNodes {
				if n.Meta["framework"] != "odoo" || n.Meta["odoo_kind"] != "route" {
					continue
				}
				p, _ := n.Meta["odoo_route"].(string)
				handler, _ := n.Meta["odoo_handler"].(string)
				if p == "" || handler == "" {
					continue
				}
				var methods []string
				b, _ := json.Marshal(n.Meta["odoo_methods"])
				_ = json.Unmarshal(b, &methods)
				if len(methods) == 0 {
					methods = []string{"ANY"}
				}
				p, params := NormalizeHTTPPathWithParams(odooPathConverter.ReplaceAllString(p, "{$1}"))
				for _, method := range methods {
					method = strings.ToUpper(method)
					meta := map[string]any{"method": method, "path": p, "framework": "odoo", "odoo_options": n.Meta["odoo_options"]}
					if len(params) > 0 {
						meta["path_param_names"] = params
					}
					out = append(out, Contract{ID: fmt.Sprintf("http::%s::%s", method, p), Type: ContractHTTP, Role: RoleProvider, SymbolID: handler, FilePath: ctx.FilePath, Line: n.StartLine, Meta: meta, Confidence: 0.9})
				}
			}
			return out
		},
	})
}
