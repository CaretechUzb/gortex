package languages

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Odoo module manifests.
//
// Every Odoo addon carries a `__manifest__.py` (pre-v10:
// `__openerp__.py`) whose body is a single dict literal declaring the
// module's identity, the addons it `depends` on, and the ordered list of
// XML/CSV files it loads. That file is the ONLY authority on which data
// files belong to which module and on the module dependency graph —
// nothing in the Python or XML sources states either.
//
// This is a capture pass on the ordinary Python extractor rather than an
// entry in the manifest table at Indexer.extractExternalModules, and that
// is a structural necessity, not a preference: that table iterates a
// fixed set of repo-root-relative paths (go.mod, package.json, …) with no
// directory walk, while Odoo has one manifest per addon at arbitrary
// depth. Running here also means incremental re-indexing works with no
// extra plumbing — a manifest is just a .py file that changed.

// odooManifestBasenames are the manifest filenames across Odoo versions.
var odooManifestBasenames = map[string]bool{
	"__manifest__.py": true,
	"__openerp__.py":  true,
}

// odooModuleNodeID builds the KindModule ID for an addon, following the
// documented `module::<ecosystem>:<name>@<version>` convention.
func odooModuleNodeID(name, version string) string {
	return "module::odoo:" + name + "@" + version
}

// captureOdooManifest reads an addon's __manifest__.py and emits the
// module node, its dependency edges, and its data-file edges.
func captureOdooManifest(result *parser.ExtractionResult, root *sitter.Node, filePath string, src []byte) {
	if result == nil || root == nil || !odooManifestBasenames[path.Base(filePath)] {
		return
	}
	dict := odooManifestDict(root)
	if dict == nil {
		return
	}
	line := int(dict.StartPoint().Row) + 1
	entries := odooManifestEntries(dict, src)

	// The module's identity is its DIRECTORY name — that is what
	// `depends` entries and external IDs name. The manifest's own "name"
	// key is a human-facing label ("Sales"), not an identifier.
	//
	// A repository that IS a single addon has no directory segment in
	// its repo-relative paths (the manifest is just `__manifest__.py`),
	// so there is nothing to read. Rather than drop the module node —
	// losing the dependency graph and the data-file list entirely — fall
	// back to a slug of the display name and record that the identity
	// was derived rather than read.
	moduleName, derivation := path.Base(path.Dir(filePath)), "directory"
	if moduleName == "" || moduleName == "." || moduleName == "/" {
		moduleName, derivation = odooSlug(odooManifestString(entries["name"], src)), "manifest_name"
	}
	if moduleName == "" {
		return
	}
	version := odooManifestString(entries["version"], src)
	moduleID := odooModuleNodeID(moduleName, version)

	meta := map[string]any{
		"ecosystem":              "odoo",
		"odoo_module":            moduleName,
		"odoo_module_derivation": derivation,
	}
	if label := odooManifestString(entries["name"], src); label != "" {
		meta["odoo_display_name"] = label
	}
	for _, key := range []string{"version", "license", "category", "summary"} {
		if v := odooManifestString(entries[key], src); v != "" {
			meta["odoo_"+key] = v
		}
	}
	for _, key := range []string{"application", "auto_install", "installable"} {
		if n := entries[key]; n != nil {
			meta["odoo_"+key] = strings.TrimSpace(n.Content(src)) == "True"
		}
	}

	result.Nodes = append(result.Nodes, &graph.Node{
		ID: moduleID, Kind: graph.KindModule, Name: moduleName,
		FilePath: filePath, StartLine: line, EndLine: line,
		Language: "python", Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: filePath, To: moduleID, Kind: graph.EdgeDefines,
		FilePath: filePath, Line: line,
	})

	// depends: the addon dependency graph. The target is unversioned
	// because the manifest never states a dependency's version — the
	// same shape external module dependencies already use.
	for _, dep := range odooManifestStringList(entries["depends"], src) {
		result.Edges = append(result.Edges, &graph.Edge{
			From: moduleID, To: odooModuleNodeID(dep, ""), Kind: graph.EdgeDependsOnModule,
			FilePath: filePath, Line: line, Origin: graph.OriginASTResolved,
			Meta: map[string]any{"ecosystem": "odoo", "odoo_depends": dep},
		})
	}

	// data / demo: the file list, resolvable to a real path right here —
	// no placeholder and no resolver needed, because the manifest path is
	// relative to the manifest's own directory.
	dir := path.Dir(filePath)
	for _, key := range []string{"data", "demo"} {
		for _, rel := range odooManifestStringList(entries[key], src) {
			result.Edges = append(result.Edges, &graph.Edge{
				From: moduleID, To: path.Join(dir, rel), Kind: graph.EdgeReferences,
				FilePath: filePath, Line: line, Origin: graph.OriginASTResolved,
				Meta: map[string]any{"odoo_link": key},
			})
		}
	}

	odooManifestAssets(result, entries["assets"], src, moduleID, filePath, dir, line)
}

// odooManifestAssets walks the `assets` dict — bundle name → list of
// paths. Glob entries and operator tuples (('remove', …), ('replace', …))
// are recorded in Meta but produce no edge, since they name no single
// resolvable file.
func odooManifestAssets(result *parser.ExtractionResult, assets *sitter.Node, src []byte, moduleID, filePath, dir string, line int) {
	if assets == nil || assets.Type() != "dictionary" {
		return
	}
	for i, nc := 0, int(assets.NamedChildCount()); i < nc; i++ {
		pair := assets.NamedChild(i)
		if pair == nil || pair.Type() != "pair" {
			continue
		}
		bundle := odooManifestString(pair.ChildByFieldName("key"), src)
		if bundle == "" {
			continue
		}
		for _, rel := range odooManifestStringList(pair.ChildByFieldName("value"), src) {
			meta := map[string]any{"odoo_link": "asset", "odoo_bundle": bundle}
			if strings.ContainsAny(rel, "*?") {
				// A glob names a set, not a file; record it so the
				// bundle's shape stays visible without minting an edge
				// to a path that does not exist.
				meta["odoo_asset_glob"] = rel
				continue
			}
			result.Edges = append(result.Edges, &graph.Edge{
				From: moduleID, To: odooAssetPath(dir, rel), Kind: graph.EdgeReferences,
				FilePath: filePath, Line: line, Origin: graph.OriginASTResolved,
				Meta: meta,
			})
		}
	}
}

// odooAssetPath resolves an asset entry. Asset paths are written
// module-relative with the module name as the first segment
// ("sale/static/src/js/x.js"), so the module segment is dropped before
// joining onto the addon directory.
func odooAssetPath(dir, rel string) string {
	moduleName := path.Base(dir)
	if trimmed := strings.TrimPrefix(rel, moduleName+"/"); trimmed != rel {
		return path.Join(dir, trimmed)
	}
	return path.Join(dir, rel)
}

// odooSlug reduces a human label to an identifier-shaped fallback name.
func odooSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		case r == ' ', r == '-', r == '.', r == '/':
			b.WriteByte('_')
		}
	}
	return strings.Trim(b.String(), "_")
}

// odooManifestDict finds the manifest's top-level dict literal. The file
// body is a bare expression, so the dict is the module's only statement.
func odooManifestDict(root *sitter.Node) *sitter.Node {
	for i, nc := 0, int(root.NamedChildCount()); i < nc; i++ {
		stmt := root.NamedChild(i)
		if stmt == nil || stmt.Type() != "expression_statement" {
			continue
		}
		for j, jc := 0, int(stmt.NamedChildCount()); j < jc; j++ {
			if c := stmt.NamedChild(j); c != nil && c.Type() == "dictionary" {
				return c
			}
		}
	}
	return nil
}

// odooManifestEntries indexes the manifest dict by key.
func odooManifestEntries(dict *sitter.Node, src []byte) map[string]*sitter.Node {
	out := map[string]*sitter.Node{}
	for i, nc := 0, int(dict.NamedChildCount()); i < nc; i++ {
		pair := dict.NamedChild(i)
		if pair == nil || pair.Type() != "pair" {
			continue
		}
		if key := odooManifestString(pair.ChildByFieldName("key"), src); key != "" {
			out[key] = pair.ChildByFieldName("value")
		}
	}
	return out
}

func odooManifestString(n *sitter.Node, src []byte) string {
	if n == nil || n.Type() != "string" {
		return ""
	}
	return pyStringLiteralValue(n.Content(src))
}

// odooManifestStringList reads a list/tuple of string literals, skipping
// non-string entries such as the ('remove', …) operator tuples that may
// appear in an assets bundle.
func odooManifestStringList(n *sitter.Node, src []byte) []string {
	if n == nil || (n.Type() != "list" && n.Type() != "tuple") {
		return nil
	}
	var out []string
	for i, nc := 0, int(n.NamedChildCount()); i < nc; i++ {
		if v := odooManifestString(n.NamedChild(i), src); v != "" {
			out = append(out, v)
		}
	}
	return out
}
