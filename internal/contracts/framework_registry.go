package contracts

import (
	"bytes"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// Framework route-pass registry. The per-line httpPatterns table handles the
// simple `app.get('/x', h)` route shapes; the *structural* route passes
// (Django urlpatterns, DRF routers, Rails resources, file-based routes,
// Flask/Express object forms) need cross-symbol resolution and run as
// node-aware passes. This registry is the single front door over those
// passes: it gives each one a name, a language filter, a crash-isolated
// Detect pre-filter, and an Extract step, and lets a new framework register
// without editing http.go.
//
// This is the *extract-time* route registry. Its sibling, the post-resolution
// cross-language dispatch synthesizer registry, lives in
// internal/resolver/framework_synth.go (defaultFrameworkSynthesizers); the
// two are a pair — one binds routes as Contracts at extract time, the other
// synthesizes dynamic-dispatch call edges after resolution.

// RouteExtractCtx bundles everything HTTPExtractor.extract threads into a
// node-aware route pass.
type RouteExtractCtx struct {
	FilePath  string
	Src       []byte
	Text      string
	Lines     []string
	FileNodes []*graph.Node
	Lang      string
	Tree      *parser.ParseTree
	H         *HTTPExtractor
}

// FrameworkRoutePass is one registered structural route extractor.
type FrameworkRoutePass interface {
	// Name is a stable tag (e.g. "django", "rails").
	Name() string
	// Languages returns the languages this pass applies to; empty = all.
	Languages() []string
	// Detect is a cheap pre-filter on the file path + source.
	Detect(filePath string, src []byte) bool
	// Extract emits the pass's route Contracts.
	Extract(ctx *RouteExtractCtx) []Contract
}

// routePass adapts a function set into a FrameworkRoutePass, so the existing
// node-aware passes register without a bespoke type each.
type routePass struct {
	name   string
	langs  []string
	detect func(filePath string, src []byte) bool
	run    func(h *HTTPExtractor, ctx *RouteExtractCtx) []Contract
}

func (p *routePass) Name() string        { return p.name }
func (p *routePass) Languages() []string { return p.langs }
func (p *routePass) Detect(filePath string, src []byte) bool {
	return p.detect != nil && p.detect(filePath, src)
}
func (p *routePass) Extract(ctx *RouteExtractCtx) []Contract {
	if ctx == nil || ctx.H == nil || p.run == nil {
		return nil
	}
	return p.run(ctx.H, ctx)
}

// frameworkRoutePasses is the ordered registry; ordering is registration
// order and is deterministic.
var frameworkRoutePasses []FrameworkRoutePass

// RegisterFrameworkRoutePass adds a route pass to the registry. This is the
// public extension point — a new framework registers from an init() with no
// edits to http.go.
func RegisterFrameworkRoutePass(p FrameworkRoutePass) {
	if p != nil {
		frameworkRoutePasses = append(frameworkRoutePasses, p)
	}
}

// RegisteredFrameworkRoutePasses returns the registry in order.
func RegisteredFrameworkRoutePasses() []FrameworkRoutePass {
	out := make([]FrameworkRoutePass, len(frameworkRoutePasses))
	copy(out, frameworkRoutePasses)
	return out
}

// ApplicableFrameworkRoutePasses returns the registered passes whose language
// filter admits lang (empty Languages = all).
func ApplicableFrameworkRoutePasses(lang string) []FrameworkRoutePass {
	var out []FrameworkRoutePass
	for _, p := range frameworkRoutePasses {
		langs := p.Languages()
		if len(langs) == 0 || slices.Contains(langs, lang) {
			out = append(out, p)
		}
	}
	return out
}

// DetectFrameworks returns the names of every registered pass whose Detect
// claims the file. Each Detect is crash-isolated, so one panicking pass does
// not abort the rest.
func DetectFrameworks(filePath string, src []byte) []string {
	var out []string
	for _, p := range frameworkRoutePasses {
		if safeFrameworkDetect(p, filePath, src) {
			out = append(out, p.Name())
		}
	}
	return out
}

// safeFrameworkDetect runs a pass's Detect with a panic firewall, mirroring
// the extractor-level safeExtract isolation.
func safeFrameworkDetect(p FrameworkRoutePass, filePath string, src []byte) (claimed bool) {
	defer func() {
		if recover() != nil {
			claimed = false
		}
	}()
	return p.Detect(filePath, src)
}

// safeFrameworkExtract runs a pass's Extract with a panic firewall.
func safeFrameworkExtract(p FrameworkRoutePass, ctx *RouteExtractCtx) (out []Contract) {
	defer func() {
		if recover() != nil {
			out = nil
		}
	}()
	return p.Extract(ctx)
}

// runFrameworkRoutePasses offers a file to every language-applicable pass and
// returns the union of their route Contracts, crash-isolated per pass.
func runFrameworkRoutePasses(ctx *RouteExtractCtx) []Contract {
	var out []Contract
	for _, p := range ApplicableFrameworkRoutePasses(ctx.Lang) {
		// The configured allow-list is checked before Detect so an
		// excluded pass costs nothing — not even its content pre-filter.
		// The registry itself stays whole: ApplicableFrameworkRoutePasses
		// is the inventory read behind `analyze route_frameworks`, and
		// policy belongs at the driver, not in a registry query.
		if ctx.H != nil && !ctx.H.AllowedFrameworks.Allows(p.Name()) {
			continue
		}
		if !safeFrameworkDetect(p, ctx.FilePath, ctx.Src) {
			continue
		}
		out = append(out, safeFrameworkExtract(p, ctx)...)
	}
	return out
}

func init() {
	// Structural route passes, in their historical run order. The httpPatterns
	// table (per-line regex routes) and the Go-AST / React-Router / client-alias
	// passes stay hard-wired in extract — they are not structural framework
	// passes keyed by a content/path pre-filter.
	RegisterFrameworkRoutePass(&routePass{
		name: "file-based", langs: nil,
		detect: func(filePath string, _ []byte) bool { return isFileBasedRouteFile(filePath) },
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractFileBasedRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	RegisterFrameworkRoutePass(&routePass{
		name: "flask-restful", langs: []string{"python"},
		detect: func(_ string, src []byte) bool { return bytes.Contains(src, []byte("add_resource")) },
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractFlaskRestfulRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	RegisterFrameworkRoutePass(&routePass{
		name: "flask-add-url-rule", langs: []string{"python"},
		detect: func(_ string, src []byte) bool { return bytes.Contains(src, []byte("add_url_rule")) },
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractFlaskAddURLRule(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	RegisterFrameworkRoutePass(&routePass{
		name: "django", langs: []string{"python"},
		detect: func(_ string, src []byte) bool { return djangoRouteCallRE.Match(src) },
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractDjangoRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	RegisterFrameworkRoutePass(&routePass{
		name: "drf", langs: []string{"python"},
		detect: func(_ string, src []byte) bool { return bytes.Contains(src, []byte(".register(")) },
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractDRFRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	RegisterFrameworkRoutePass(&routePass{
		name: "flask-decorator", langs: []string{"python"},
		detect: func(_ string, src []byte) bool { return bytes.Contains(src, []byte(".route(")) },
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractFlaskDecoratorRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	// Odoo controller routes. Detection keys on the decorator rather than
	// on an import, because `from odoo.http import route` lets a
	// controller write the bare @route(...) form.
	RegisterFrameworkRoutePass(&routePass{
		name: "odoo", langs: []string{"python"},
		detect: func(_ string, src []byte) bool { return odooRouteDecoratorRE.Match(src) },
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractOdooRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	RegisterFrameworkRoutePass(&routePass{
		name: "rails-resources", langs: []string{"ruby"},
		detect: func(_ string, src []byte) bool { return bytes.Contains(src, []byte("resource")) },
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractRailsResourceRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	RegisterFrameworkRoutePass(&routePass{
		name: "laravel-resources", langs: []string{"php"},
		detect: func(_ string, src []byte) bool {
			return bytes.Contains(src, []byte("Route::resource")) || bytes.Contains(src, []byte("Route::apiResource"))
		},
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractLaravelResourceRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	// Drupal *.routing.yml routes (langs nil: gated by filename, not language).
	RegisterFrameworkRoutePass(&routePass{
		name: "drupal", langs: nil,
		detect: func(filePath string, _ []byte) bool {
			return strings.HasSuffix(filePath, ".routing.yml") || strings.HasSuffix(filePath, ".routing.yaml")
		},
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractDrupalRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	// Rust Axum chained-method `.route(...)` + Actix builder
	// (`web::resource(...).route(web::get().to(h))`) routes, with `web::scope`
	// prefix joining.
	RegisterFrameworkRoutePass(&routePass{
		name: "rust-routes", langs: []string{"rust"},
		detect: func(_ string, src []byte) bool {
			return bytes.Contains(src, []byte(".route(")) || bytes.Contains(src, []byte("web::resource"))
		},
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractRustRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	// Play Framework conf/routes (langs nil: gated by filename, not language —
	// the file is extensionless).
	RegisterFrameworkRoutePass(&routePass{
		name: "play", langs: nil,
		detect: func(filePath string, _ []byte) bool {
			return strings.HasSuffix(filePath, "conf/routes") || strings.HasSuffix(filePath, ".routes")
		},
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractPlayRoutes(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
	RegisterFrameworkRoutePass(&routePass{
		name: "express-objects", langs: []string{"typescript", "javascript"},
		detect: func(_ string, src []byte) bool { return bytes.Contains(src, []byte(".route(")) },
		run: func(h *HTTPExtractor, c *RouteExtractCtx) []Contract {
			return h.extractObjectRouteProviders(c.FilePath, c.Text, c.Lines, c.FileNodes, c.Lang, c.Tree)
		},
	})
}
