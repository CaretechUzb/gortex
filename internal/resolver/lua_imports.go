package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// resolveLuaRequires binds Lua / Luau `require(...)` import edges that the
// extractor left on `unresolved::import::<module>` placeholders onto the
// indexed module file they reference.
//
// The Lua extractor carries the full dotted module path for a classic string
// require (`require("a.b.c")` → target `a.b.c`) and the leaf module name plus
// a `roblox_path` for an instance-path require (`require(script.Parent.Foo)` →
// target `Foo`, with `roblox_path` set). Classic requires map the dotted path
// to candidate files (`a/b/c.lua`, the `.luau` form, then the `a/b/c/init.*`
// directory module), the direct repo-relative path winning, then a unique
// path-suffix match (the package-root net). Roblox instance-path requires bind
// to the unique `<leaf>.lua` / `<leaf>.luau` file in the repo, refusing on
// ambiguity. Edges whose target is not indexed stay external.
//
// Runs serially in ResolveAll's relative-import settle window, after
// resolveRelativeImports (which never touches Lua).
func (r *Resolver) resolveLuaRequires() {
	if !r.graphHasLanguage("lua") && !r.graphHasLanguage("luau") {
		return
	}
	fileLang := make(map[string]string, 1024)
	fileIDs := make(map[string]struct{}, 1024)
	// filesByBase indexes every KindFile by basename for the suffix nets.
	filesByBase := make(map[string][]string, 1024)
	for file := range graph.FileLanguageNodesSeq(r.graph) {
		if file.ID == "" {
			continue
		}
		fileLang[file.ID] = file.Language
		fileIDs[file.ID] = struct{}{}
		base := file.ID
		if i := strings.LastIndex(file.ID, "/"); i >= 0 {
			base = file.ID[i+1:]
		}
		filesByBase[base] = append(filesByBase[base], file.ID)
	}

	var reindexBatch []graph.EdgeReindex
	for e := range r.graph.EdgesByKind(graph.EdgeImports) {
		if e == nil || !strings.HasPrefix(e.To, "unresolved::import::") {
			continue
		}
		// Scoped warm pass: an unchanged repo's requires were already bound by a
		// prior full pass, so only reconsider the changed repos' imports.
		if !r.edgeFromInScope(e.From) {
			continue
		}
		if lang := fileLang[e.From]; lang != "lua" && lang != "luau" {
			continue
		}
		name := strings.TrimPrefix(e.To, "unresolved::import::")
		if name == "" {
			continue
		}
		var resolved string
		if _, isRoblox := e.Meta["roblox_path"]; isRoblox {
			resolved = resolveLuaRobloxRequire(filesByBase, name)
		} else {
			resolved = resolveLuaModuleRequire(fileIDs, filesByBase, name)
		}
		if resolved == "" {
			continue
		}
		oldTo := e.To
		e.To = resolved
		e.Origin = graph.OriginASTResolved
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	}
	if len(reindexBatch) > 0 {
		r.graph.ReindexEdges(reindexBatch)
	}
}

// luaModuleCandidates converts a dotted/slashed Lua module path to its ordered
// candidate file paths: the `.lua` / `.luau` file, then the `init` directory
// module.
func luaModuleCandidates(modPath string) []string {
	p := strings.Trim(strings.ReplaceAll(modPath, ".", "/"), "/")
	if p == "" {
		return nil
	}
	return []string{p + ".lua", p + ".luau", p + "/init.lua", p + "/init.luau"}
}

// resolveLuaModuleRequire resolves a classic string require's dotted module
// path: the direct repo-relative candidate file wins; otherwise a unique
// path-suffix match (handling package-root-prefixed layouts) wins, refusing on
// ambiguity.
func resolveLuaModuleRequire(fileIDs map[string]struct{}, filesByBase map[string][]string, modPath string) string {
	cands := luaModuleCandidates(modPath)
	for _, c := range cands {
		if _, ok := fileIDs[c]; ok {
			return c
		}
	}
	for _, c := range cands {
		if m := luaUniqueSuffixMatch(filesByBase, c); m != "" {
			return m
		}
	}
	return ""
}

// luaUniqueSuffixMatch returns the single indexed file equal to, or ending with
// `/`+path, or "" when there is none or more than one (ambiguous).
func luaUniqueSuffixMatch(filesByBase map[string][]string, path string) string {
	base := path
	if i := strings.LastIndex(path, "/"); i >= 0 {
		base = path[i+1:]
	}
	suffix := "/" + path
	match := ""
	for _, cand := range filesByBase[base] {
		if cand == path || strings.HasSuffix(cand, suffix) {
			if match != "" && match != cand {
				return "" // ambiguous across roots
			}
			match = cand
		}
	}
	return match
}

// resolveLuaRobloxRequire binds a Roblox instance-path require by its leaf
// module name to the unique indexed `<leaf>.lua` / `<leaf>.luau` file, refusing
// on ambiguity.
func resolveLuaRobloxRequire(filesByBase map[string][]string, leaf string) string {
	match := ""
	for _, ext := range []string{".lua", ".luau"} {
		for _, cand := range filesByBase[leaf+ext] {
			if match != "" && match != cand {
				return "" // ambiguous module name in the repo
			}
			match = cand
		}
	}
	return match
}
