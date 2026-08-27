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
func bindOdooJS(g graph.Store, scope map[string]bool) int {
	byModule := odooJSModuleIndex(g)
	byTemplate := odooTemplateIndex(g)
	byJSMethod := odooJSMethodIndex(g)

	var plans []odooBindPlan
	odooCollect(g, scope, odooJSVia, odooJSEdgeKinds, func(e *graph.Edge) {
		if spec := odooMetaString(e, "odoo_js_import"); spec != "" {
			plans = append(plans, odooBindPlan{
				edge:        e,
				target:      odooResolveJSModule(byModule, spec),
				placeholder: odooJSModuleStubPrefix + spec,
			})
			return
		}
		if tmpl := odooMetaString(e, "odoo_template"); tmpl != "" {
			plans = append(plans, odooBindPlan{
				edge:        e,
				target:      odooLookupXMLID(byTemplate, tmpl),
				placeholder: odooTemplateStubPrefix + tmpl,
			})
			return
		}
		if method := odooMetaString(e, "odoo_patch_method"); method != "" {
			target := odooMetaString(e, "odoo_patch_target")
			if target == "" {
				return
			}
			key := target + "." + method
			plans = append(plans, odooBindPlan{
				edge: e, target: byJSMethod[key], placeholder: odooJSMethodStubPrefix + key,
			})
		}
	})
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
func odooResolveJSModule(byModule map[string]string, spec string) string {
	if spec == "" {
		return ""
	}
	if id := byModule[spec]; id != "" {
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
	if id := byModule[addon+"/"+rest]; id != "" {
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
	return byModule[addon+"/static/"+cleaned]
}

// odooJSModuleIndex maps a module specifier to the file node providing
// it, under both vocabularies odooResolveJSModule accepts.
//
// Modern modules are keyed "<addon>/<module-relative path>": Odoo client
// assets always live at `<addon>/static/src/<rest>`, so the key is
// recovered by stripping that prefix and the file extension. Legacy
// modules are keyed on the name the file passed to `odoo.define(...)`,
// which the extractor captured onto the file node — nothing about the
// path implies it. The two key shapes cannot collide: a legacy name is
// dotted and a modern key is slashed.
func odooJSModuleIndex(g graph.Store) map[string]string {
	out := map[string]string{}
	put := func(key, id string) {
		if key == "" {
			return
		}
		if prev, exists := out[key]; !exists || id < prev {
			out[key] = id
		}
	}
	for n := range g.NodesByKind(graph.KindFile) {
		if n == nil || n.Meta == nil {
			continue
		}
		legacy, _ := n.Meta["odoo_js_legacy_name"].(string)
		put(legacy, n.ID)
		addon, _ := n.Meta["odoo_js_addon"].(string)
		if addon == "" {
			continue
		}
		if rest, ok := odooJSModuleRelPath(n.FilePath, addon); ok {
			put(addon+"/"+rest, n.ID)
		}
		// The escape-form key, for specifiers that climb out of
		// `static/src` (see odooResolveJSModule). Kept distinct by its
		// literal `static/` segment, which a module-relative key never
		// has.
		if rest, ok := odooJSStaticRelPath(n.FilePath, addon); ok {
			put(addon+"/static/"+rest, n.ID)
		}
	}
	return out
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

// odooTemplateIndex maps a QWeb template name to the XML node declaring
// it — the link between an OWL component and its markup.
//
// Indexed under both the qualified and bare forms for the same reason as
// odooXMLIDIndex: a `<template id="OrderWidget">` picks up its module
// prefix from the file path, while the JS side always names the template
// fully qualified (`static template = "sale.OrderWidget"`). Without the
// bare fallback the two never meet in a repository that is itself a
// single addon.
func odooTemplateIndex(g graph.Store) map[string]string {
	out := map[string]string{}
	put := func(key, id string) {
		if key == "" {
			return
		}
		if prev, ok := out[key]; !ok || id < prev {
			out[key] = id
		}
	}
	for n := range g.NodesByKind(graph.KindResource) {
		if n == nil || n.Meta == nil {
			continue
		}
		tmpl, _ := n.Meta["odoo_template"].(string)
		if tmpl == "" {
			continue
		}
		put(tmpl, n.ID)
		put(odooBareXMLID(tmpl), n.ID)
	}
	return out
}

// odooJSMethodIndex maps "<ClassName>.<method>" to the method node a
// patch(...) call overrides.
func odooJSMethodIndex(g graph.Store) map[string]string {
	classNames := map[string]string{}
	for n := range g.NodesByKind(graph.KindType) {
		if n == nil || n.Name == "" {
			continue
		}
		if n.Language != "javascript" && n.Language != "typescript" {
			continue
		}
		if prev, ok := classNames[n.ID]; !ok || n.Name < prev {
			classNames[n.ID] = n.Name
		}
	}
	if len(classNames) == 0 {
		return nil
	}
	out := map[string]string{}
	for n := range g.NodesByKind(graph.KindMethod) {
		if n == nil || n.Name == "" {
			continue
		}
		idx := strings.LastIndex(n.ID, ".")
		if idx <= 0 {
			continue
		}
		className := classNames[n.ID[:idx]]
		if className == "" {
			continue
		}
		key := className + "." + n.Name
		if prev, ok := out[key]; !ok || n.ID < prev {
			out[key] = n.ID
		}
	}
	return out
}
