package resolver

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Odoo front-end (OWL) binding.
//
// NOTE: this file is deliberately NOT named odoo_js.go. Go reads a
// trailing `_js` as the GOOS build constraint for js/wasm, so that name
// would silently exclude the file from every ordinary build — the
// package would compile everywhere except where it was needed.

// odooJSEdgeKinds are the kinds an `odoo-js` placeholder rides.
var odooJSEdgeKinds = []graph.EdgeKind{
	graph.EdgeImports,
	graph.EdgeRendersChild,
	graph.EdgeOverrides,
	graph.EdgeReferences,
}

// bindOdooJS binds the three Odoo front-end placeholder families:
// addon-aliased module imports, OWL component → QWeb template links, and
// patched method overrides.
func bindOdooJS(g graph.Store, edges []*graph.Edge, sc *odooSiblingCache, d *odooDecls) int {
	byModule := d.jsModules
	byTemplate := d.templates
	byJSMethod := d.jsMethods

	var plans []odooBindPlan
	for _, e := range edges {
		if spec := odooMetaString(e, "odoo_js_import"); spec != "" {
			plans = append(plans, odooBindPlan{
				edge:        e,
				target:      odooResolveJSModule(sc, byModule, e.From, spec),
				placeholder: odooJSModuleStubPrefix + spec,
			})
			continue
		}
		if tmpl := odooMetaString(e, "odoo_template"); tmpl != "" {
			plans = append(plans, odooBindPlan{
				edge:        e,
				target:      odooLookupXMLID(sc, byTemplate, e.From, tmpl),
				placeholder: odooTemplateStubPrefix + tmpl,
			})
			continue
		}
		if method := odooMetaString(e, "odoo_patch_method"); method != "" {
			target := odooMetaString(e, "odoo_patch_target")
			if target == "" {
				continue
			}
			key := target + "." + method
			plans = append(plans, odooBindPlan{
				edge:        e,
				target:      byJSMethod.lookup(sc, e.From, key),
				placeholder: odooJSMethodStubPrefix + key,
			})
		}
	}
	return odooRebind(g, plans, ConfidenceTyped)
}

// odooResolveJSModule turns a module specifier into a real file node.
//
// Two vocabularies share this placeholder. A legacy `require("web.core")`
// names a module the way `odoo.define("web.core", …)` declared it: an
// arbitrary string that no path rewriting can reconstruct, so it is
// matched against the declared names directly and is tried first.
//
// A modern `@web/core/registry` denotes
// `<somewhere>/web/static/src/core/registry.js`, but where that
// `<somewhere>` is depends on the deployment's addons layout, so it
// cannot be computed from the specifier alone. That half of the index is
// keyed on the (addon, module-relative path) pair every Odoo JS file node
// already carries, which makes the join exact rather than a guess.
func odooResolveJSModule(sc *odooSiblingCache, byModule odooIndex, fromID, spec string) string {
	if spec == "" {
		return ""
	}
	if id := byModule.lookup(sc, fromID, spec); id != "" {
		return id
	}
	spec = strings.TrimPrefix(spec, "@")
	if spec == "" {
		return ""
	}
	// The `@odoo/owl` framework package is not an addon and has no file
	// in the graph; leaving it unbound is correct.
	if strings.HasPrefix(spec, "odoo/") {
		return ""
	}
	addon, rest, ok := strings.Cut(spec, "/")
	if !ok || addon == "" || rest == "" {
		return ""
	}
	if id := byModule.lookup(sc, fromID, addon+"/"+rest); id != "" {
		return id
	}
	// `@web/../tests/helpers/utils` escapes the addon's `static/src` root.
	// Odoo's asset loader treats the specifier as a path relative to that
	// root, so a `..` segment climbs to `static/` — which is how every test
	// module addresses the helpers living beside `src/` rather than in it.
	// Resolving the escape against the addon-relative key alone cannot
	// work: the `..` never matches a file whose path has no such segment.
	if !strings.Contains(rest, "../") {
		return ""
	}
	cleaned := path.Clean("src/" + rest)
	if strings.HasPrefix(cleaned, "..") {
		return "" // climbed out of the addon entirely
	}
	return byModule.lookup(sc, fromID, addon+"/static/"+cleaned)
}

// odooJSModuleRelPath extracts the part of a path after
// `<addon>/static/src/`, without its extension.
func odooJSModuleRelPath(filePath, addon string) (string, bool) {
	return odooJSPathAfter(filePath, "/"+addon+"/static/src/")
}

// odooJSStaticRelPath extracts the part of a path after `<addon>/static/`,
// without its extension — the reach of a specifier that climbs out of
// `static/src`, which is where Odoo keeps its JS test helpers.
func odooJSStaticRelPath(filePath, addon string) (string, bool) {
	return odooJSPathAfter(filePath, "/"+addon+"/static/")
}

// odooJSPathAfter returns the extension-less remainder of filePath after
// marker, which is anchored on a leading slash so an addon name never
// matches mid-segment.
func odooJSPathAfter(filePath, marker string) (string, bool) {
	idx := strings.Index("/"+filePath, marker)
	if idx < 0 {
		return "", false
	}
	rest := ("/" + filePath)[idx+len(marker):]
	if rest == "" {
		return "", false
	}
	for _, ext := range []string{".js", ".ts", ".mjs"} {
		if strings.HasSuffix(rest, ext) {
			return strings.TrimSuffix(rest, ext), true
		}
	}
	return rest, true
}
