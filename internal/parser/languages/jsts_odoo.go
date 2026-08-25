package languages

import (
	"bytes"
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Odoo OWL / JavaScript front end.
//
// Odoo's client is ES modules marked by a `/** @odoo-module **/` header
// (pre-v15: an `odoo.define("web.Foo", …)` wrapper). Three things in it
// are invisible to a generic JS extractor:
//
//   - Cross-addon imports use an addon alias — `@web/core/registry` — not
//     a relative path, so nothing links the importer to the file it imports.
//   - Components, services, actions and fields are registered by STRING key
//     into a runtime registry (`registry.category("actions").add("x", C)`)
//     and consumed by that string (`useService("orm")`), so the wiring
//     between provider and consumer exists only at runtime.
//   - An OWL component names its markup by string
//     (`static template = "sale.OrderWidget"`), which is the only link
//     between a JS class and the QWeb XML that renders it.
//
// This pass is a capture on the existing JS/TS extractors rather than a
// separate extractor: Odoo JS is ordinary ES module syntax, and the
// jsts_* capture idiom (see jsts_vuex_dispatch.go) is what every other
// framework here uses. It is invoked from both TypeScriptExtractor.Extract
// and JavaScriptExtractor.Extract.

// odooJSVia tags the placeholder edges this pass emits.
const odooJSVia = "odoo-js"

// Odoo JS placeholder prefixes. The template prefix deliberately matches
// the XML extractor's template node naming so the resolver can bind a
// component to its QWeb markup.
const (
	odooJSModulePlaceholder   = "unresolved::odoo::jsmodule::"
	odooTemplatePlaceholder   = "unresolved::odoo::template::"
	odooJSOverridePlaceholder = "unresolved::odoo::jsmethod::"
)

// odooRegistryNodeID is the shared identity for one registry slot, so a
// provider (`.add("orm", …)`) and a consumer (`useService("orm")`) meet on
// the same node with no resolution pass involved.
func odooRegistryNodeID(category, key string) string {
	return "odoo::registry::" + category + "::" + key
}

// captureOdooModule tags an Odoo JS module and emits its registry,
// service, template and patch links.
//
// The gate is a byte scan before any AST walk. `/** @odoo-module **/` is
// mandatory on every module file from Odoo 15 onward, which makes this a
// far stronger and cheaper signal than the Python side has.
func captureOdooModule(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if root == nil || result == nil {
		return
	}
	legacy := bytes.Contains(src, []byte("odoo.define("))
	if !bytes.Contains(src, []byte("@odoo-module")) && !legacy {
		return
	}

	fileNode := odooFindNode(result, filePath)
	if fileNode == nil {
		return
	}
	if fileNode.Meta == nil {
		fileNode.Meta = map[string]any{}
	}
	fileNode.Meta["odoo_js_module"] = true
	if addon := odooJSAddonFromPath(filePath); addon != "" {
		fileNode.Meta["odoo_js_addon"] = addon
	}
	if legacy {
		fileNode.Meta["odoo_js_legacy"] = true
	}

	funcRanges := buildFuncRanges(result)
	enclosing := func(line int) string {
		if id := findEnclosingFunc(funcRanges, line); id != "" {
			return id
		}
		return filePath
	}

	odooJSWalk(root, func(n *sitter.Node) {
		switch n.Type() {
		case "import_statement":
			odooJSImport(result, n, src, filePath)
		case "call_expression":
			odooJSCall(result, n, src, filePath, enclosing)
		case "class_declaration", "class":
			odooJSComponent(result, n, src, filePath)
		}
	})
}

// odooJSImport records an addon-aliased import as a placeholder. The
// alias cannot be rewritten to a path here: `@web/core/registry` lives at
// `<somewhere>/web/static/src/core/registry.js`, and where that
// `<somewhere>` is depends on the deployment's addons layout, which one
// file cannot see. The resolver binds it by matching the suffix against
// files already tagged with their addon.
func odooJSImport(result *parser.ExtractionResult, n *sitter.Node, src []byte, filePath string) {
	sourceNode := n.ChildByFieldName("source")
	if sourceNode == nil {
		return
	}
	spec := strings.Trim(strings.TrimSpace(sourceNode.Content(src)), "\"'`")
	if !strings.HasPrefix(spec, "@") {
		return
	}
	line := int(n.StartPoint().Row) + 1
	result.Edges = append(result.Edges, &graph.Edge{
		From: filePath, To: odooJSModulePlaceholder + spec, Kind: graph.EdgeImports,
		FilePath: filePath, Line: line, Origin: graph.OriginASTInferred,
		Meta: map[string]any{"via": odooJSVia, "odoo_js_import": spec},
	})
}

// odooJSCall handles the four call shapes that carry Odoo wiring.
func odooJSCall(result *parser.ExtractionResult, call *sitter.Node, src []byte, filePath string, enclosing func(int) string) {
	line := int(call.StartPoint().Row) + 1
	callee := jsCalleeLastName(call.ChildByFieldName("function"), src)
	args := call.ChildByFieldName("arguments")

	switch callee {
	case "add":
		// registry.category("actions").add("key", Component)
		category := odooRegistryCategory(call, src)
		if category == "" {
			return
		}
		strs := odooJSStringArgs(args, src)
		if len(strs) == 0 {
			return
		}
		key := strs[0]
		regID := odooRegistryNodeID(category, key)
		odooEnsureRegistryNode(result, regID, category, key, filePath, line)
		result.Edges = append(result.Edges, &graph.Edge{
			From: enclosing(line), To: regID, Kind: graph.EdgeProvides,
			FilePath: filePath, Line: line, Origin: graph.OriginASTResolved,
			Meta: map[string]any{"odoo_registry_category": category, "odoo_registry_key": key},
		})
		// The registered value is usually a local identifier, which the
		// generic resolver binds without help from us.
		if ident := odooRegistryValueName(args, src); ident != "" {
			result.Edges = append(result.Edges, &graph.Edge{
				From: regID, To: ident, Kind: graph.EdgeReferences,
				FilePath: filePath, Line: line, Origin: graph.OriginASTInferred,
				Meta: map[string]any{"via": odooJSVia, "odoo_link": "registry_value"},
			})
		}
	case "useService":
		// A service consumer meets its provider on the shared registry
		// node, so this needs no resolution pass at all.
		strs := odooJSStringArgs(args, src)
		if len(strs) == 0 {
			return
		}
		result.Edges = append(result.Edges, &graph.Edge{
			From: enclosing(line), To: odooRegistryNodeID("services", strs[0]),
			Kind: graph.EdgeConsumes, FilePath: filePath, Line: line,
			Origin: graph.OriginASTResolved,
			Meta:   map[string]any{"odoo_registry_category": "services", "odoo_registry_key": strs[0]},
		})
	case "patch":
		odooJSPatch(result, args, src, filePath, line, enclosing)
	case "define":
		// Legacy odoo.define("web.Foo", …) / require("web.Bar").
		if strs := odooJSStringArgs(args, src); len(strs) > 0 {
			if fn := odooFindNode(result, filePath); fn != nil && fn.Meta != nil {
				fn.Meta["odoo_js_legacy_name"] = strs[0]
			}
		}
	case "require":
		if strs := odooJSStringArgs(args, src); len(strs) > 0 && strings.Contains(strs[0], ".") {
			result.Edges = append(result.Edges, &graph.Edge{
				From: filePath, To: odooJSModulePlaceholder + strs[0], Kind: graph.EdgeImports,
				FilePath: filePath, Line: line, Origin: graph.OriginASTInferred,
				Meta: map[string]any{"via": odooJSVia, "odoo_js_import": strs[0], "odoo_js_legacy": true},
			})
		}
	}
}

// odooJSPatch models `patch(Target.prototype, {...})`: each key of the
// patch object overrides a method on the target.
func odooJSPatch(result *parser.ExtractionResult, args *sitter.Node, src []byte, filePath string, line int, enclosing func(int) string) {
	if args == nil {
		return
	}
	var target string
	var obj *sitter.Node
	for i, nc := 0, int(args.NamedChildCount()); i < nc; i++ {
		a := args.NamedChild(i)
		if a == nil {
			continue
		}
		if target == "" && (a.Type() == "member_expression" || a.Type() == "identifier") {
			target = strings.TrimSuffix(strings.TrimSpace(a.Content(src)), ".prototype")
			continue
		}
		if a.Type() == "object" {
			obj = a
		}
	}
	if target == "" || obj == nil {
		return
	}
	from := enclosing(line)
	result.Edges = append(result.Edges, &graph.Edge{
		From: from, To: target, Kind: graph.EdgeReferences,
		FilePath: filePath, Line: line, Origin: graph.OriginASTInferred,
		Meta: map[string]any{"via": odooJSVia, "odoo_patch_target": target},
	})
	for _, method := range odooJSObjectKeys(obj, src) {
		result.Edges = append(result.Edges, &graph.Edge{
			From: from, To: odooJSOverridePlaceholder + target + "." + method,
			Kind: graph.EdgeOverrides, FilePath: filePath, Line: line,
			Origin: graph.OriginASTInferred,
			Meta: map[string]any{
				"via": odooJSVia, "odoo_patch_target": target, "odoo_patch_method": method,
			},
		})
	}
}

// odooJSComponent links an OWL component class to the QWeb template it
// renders — the only connection between the JS class and its XML markup.
func odooJSComponent(result *parser.ExtractionResult, class *sitter.Node, src []byte, filePath string) {
	body := class.ChildByFieldName("body")
	nameNode := class.ChildByFieldName("name")
	if body == nil || nameNode == nil {
		return
	}
	className := strings.TrimSpace(nameNode.Content(src))
	classNode := odooFindClassNodeByName(result, className)
	if classNode == nil {
		return
	}
	for i, nc := 0, int(body.NamedChildCount()); i < nc; i++ {
		fd := body.NamedChild(i)
		if fd == nil || fd.Type() != "field_definition" {
			continue
		}
		prop := fd.ChildByFieldName("property")
		value := fd.ChildByFieldName("value")
		if prop == nil || value == nil {
			continue
		}
		if strings.TrimSpace(prop.Content(src)) != "template" || value.Type() != "string" {
			continue
		}
		tmpl := strings.Trim(strings.TrimSpace(value.Content(src)), "\"'`")
		if tmpl == "" {
			continue
		}
		if classNode.Meta == nil {
			classNode.Meta = map[string]any{}
		}
		classNode.Meta["odoo_owl_template"] = tmpl
		result.Edges = append(result.Edges, &graph.Edge{
			From: classNode.ID, To: odooTemplatePlaceholder + tmpl,
			Kind: graph.EdgeRendersChild, FilePath: filePath,
			Line: int(fd.StartPoint().Row) + 1, Origin: graph.OriginASTInferred,
			Meta: map[string]any{"via": odooJSVia, "odoo_template": tmpl},
		})
	}
}

func odooEnsureRegistryNode(result *parser.ExtractionResult, id, category, key, filePath string, line int) {
	if odooFindNode(result, id) != nil {
		return
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindArtifact, Name: key, QualName: category + "." + key,
		FilePath: filePath, StartLine: line, EndLine: line, Language: "javascript",
		Meta: map[string]any{
			"odoo_registry_category": category,
			"odoo_registry_key":      key,
			"artifact_kind":          "odoo_registry",
		},
	})
}

// odooRegistryCategory reads the category out of the
// `registry.category("x").add(...)` chain the `.add` call hangs off.
func odooRegistryCategory(call *sitter.Node, src []byte) string {
	fn := call.ChildByFieldName("function")
	if fn == nil || fn.Type() != "member_expression" {
		return ""
	}
	obj := fn.ChildByFieldName("object")
	if obj == nil || obj.Type() != "call_expression" {
		return ""
	}
	if jsCalleeLastName(obj.ChildByFieldName("function"), src) != "category" {
		return ""
	}
	if strs := odooJSStringArgs(obj.ChildByFieldName("arguments"), src); len(strs) > 0 {
		return strs[0]
	}
	return ""
}

// odooRegistryValueName returns the identifier registered as the value —
// the second argument of `.add("key", Value)`.
func odooRegistryValueName(args *sitter.Node, src []byte) string {
	if args == nil {
		return ""
	}
	seenString := false
	for i, nc := 0, int(args.NamedChildCount()); i < nc; i++ {
		a := args.NamedChild(i)
		if a == nil {
			continue
		}
		if a.Type() == "string" {
			seenString = true
			continue
		}
		if seenString && a.Type() == "identifier" {
			return strings.TrimSpace(a.Content(src))
		}
	}
	return ""
}

// odooJSStringArgs returns the string-literal arguments of a call, in order.
func odooJSStringArgs(args *sitter.Node, src []byte) []string {
	if args == nil {
		return nil
	}
	var out []string
	for i, nc := 0, int(args.NamedChildCount()); i < nc; i++ {
		a := args.NamedChild(i)
		if a == nil || a.Type() != "string" {
			continue
		}
		if v := strings.Trim(strings.TrimSpace(a.Content(src)), "\"'`"); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// odooJSObjectKeys returns the property names of an object literal.
func odooJSObjectKeys(obj *sitter.Node, src []byte) []string {
	var out []string
	for i, nc := 0, int(obj.NamedChildCount()); i < nc; i++ {
		p := obj.NamedChild(i)
		if p == nil {
			continue
		}
		var key *sitter.Node
		switch p.Type() {
		case "pair":
			key = p.ChildByFieldName("key")
		case "method_definition":
			key = p.ChildByFieldName("name")
		}
		if key == nil {
			continue
		}
		if name := strings.Trim(strings.TrimSpace(key.Content(src)), "\"'`"); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// odooFindClassNodeByName locates the already-emitted class node.
func odooFindClassNodeByName(result *parser.ExtractionResult, name string) *graph.Node {
	for _, n := range result.Nodes {
		if n != nil && n.Kind == graph.KindType && n.Name == name {
			return n
		}
	}
	return nil
}

// odooJSAddonFromPath derives the addon a JS file belongs to. Odoo client
// assets always live under `<addon>/static/src/...`, so the segment before
// `static` is authoritative.
func odooJSAddonFromPath(filePath string) string {
	parts := strings.Split(path.Clean(filePath), "/")
	for i, p := range parts {
		if p == "static" && i > 0 {
			return parts[i-1]
		}
	}
	return ""
}

func odooJSWalk(n *sitter.Node, fn func(*sitter.Node)) {
	if n == nil {
		return
	}
	fn(n)
	for i, nc := 0, int(n.NamedChildCount()); i < nc; i++ {
		odooJSWalk(n.NamedChild(i), fn)
	}
}
