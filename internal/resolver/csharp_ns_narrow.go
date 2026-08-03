package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// C# namespace narrowing. A using directive imports a whole namespace,
// not a name, so there is no per-reference FQN to stamp at extraction
// (the PHP approach). Instead, join what the graph already holds — the
// file's EdgeImports and each type's Meta["scope_ns"] — so a bare-name
// tie no longer falls to lexicographic ID (a sibling module's type).

// csharpFileNS is a C# file's visible-namespace evidence, split the way
// the compiler consults it: enclosing namespaces are searched before
// using directives, so an own-namespace type shadows an imported one.
type csharpFileNS struct {
	enclosing map[string]struct{} // declared namespaces + their prefixes
	imported  map[string]struct{} // using-directive namespaces (incl. project-scoped globals)
	statics   map[string]struct{} // using-static class FQNs — members in scope, not the namespace
	// scoped splits the imports by the namespace scope declaring them
	// ("" = compilation unit) — C# using visibility is scope-by-scope,
	// and a using inside namespace A grants nothing to a sibling B in
	// the same file. nil on graphs extracted before the scoped stamp:
	// consumers must then fall back to the flat imported set.
	scoped map[string]map[string]struct{}
}

// csharpFileNamespaceSet returns the namespaces visible to a C# file:
// its using-directive imports (pre-resolution unresolved::import:: or
// post-resolution external:: shape — in-pass ordering must not matter)
// plus the file's own declared namespaces and their enclosing prefixes.
func (r *Resolver) csharpFileNamespaceSet(fileID string) csharpFileNS {
	r.csharpNSMu.RLock()
	ns, ok := r.csharpNSByFile[fileID]
	r.csharpNSMu.RUnlock()
	if ok {
		return ns
	}

	ns = csharpFileNS{enclosing: map[string]struct{}{}, imported: map[string]struct{}{}, statics: map[string]struct{}{}}
	// Primary using evidence is the extractor's Meta["usings"] stamp —
	// resolveImport rewrites the per-directive edges (a namespace tail
	// matching any directory basename becomes a file-node target), so
	// only the stamp is order-independent. The edge shapes below remain
	// as a fallback for graphs extracted before the stamp existed.
	if f := r.cachedGetNode(fileID); f != nil {
		for _, u := range csharpMetaStrings(f.Meta["usings"]) {
			ns.imported[u] = struct{}{}
		}
		for _, u := range csharpMetaStrings(f.Meta["using_static"]) {
			ns.statics[u] = struct{}{}
		}
		for _, su := range csharpMetaStrings(f.Meta["scoped_usings"]) {
			i := strings.Index(su, "|")
			if i < 0 || su[i+1:] == "" {
				continue
			}
			scope, name := su[:i], su[i+1:]
			if ns.scoped == nil {
				ns.scoped = map[string]map[string]struct{}{}
			}
			if ns.scoped[scope] == nil {
				ns.scoped[scope] = map[string]struct{}{}
			}
			ns.scoped[scope][name] = struct{}{}
		}
	}
	// The stamp gate must reflect the file's OWN usings only: inherited
	// globals say nothing about whether THIS file was extracted by a
	// stamp-era binary, and a pre-stamp file still needs the edge
	// fallback below for its real usings.
	hasStamp := len(ns.imported) > 0
	// Project-scoped `global using`s from the directory chain (the
	// Usings.cs convention) join the imported tier exactly like a local
	// directive would — at the compilation-unit scope, where the
	// language puts a global using.
	for _, u := range r.csharpGlobalUsingsFor(fileID) {
		ns.imported[u] = struct{}{}
		if ns.scoped != nil {
			if ns.scoped[""] == nil {
				ns.scoped[""] = map[string]struct{}{}
			}
			ns.scoped[""][u] = struct{}{}
		}
	}
	// Project-scoped `global using static` targets join the statics set
	// the same way — their extension methods are in scope for the whole
	// unit, not just the declaring file.
	for _, s := range r.csharpGlobalStaticsFor(fileID) {
		ns.statics[s] = struct{}{}
	}
	for _, e := range r.graph.GetOutEdges(fileID) {
		if e == nil {
			continue
		}
		switch e.Kind {
		case graph.EdgeImports:
			if hasStamp {
				continue
			}
			imp := e.To
			switch {
			case strings.HasPrefix(imp, "unresolved::import::"):
				imp = strings.TrimPrefix(imp, "unresolved::import::")
			case strings.HasPrefix(imp, "external::"):
				imp = strings.TrimPrefix(imp, "external::")
			default:
				continue
			}
			if imp != "" {
				ns.imported[strings.ReplaceAll(imp, "/", ".")] = struct{}{}
			}
		case graph.EdgeDefines:
			n := r.cachedGetNode(e.To)
			if n == nil {
				continue
			}
			scope, _ := n.Meta["scope_ns"].(string)
			for scope != "" {
				ns.enclosing[scope] = struct{}{}
				i := strings.LastIndex(scope, ".")
				if i < 0 {
					break
				}
				scope = scope[:i]
			}
		}
	}

	r.csharpNSMu.Lock()
	if r.csharpNSByFile == nil {
		r.csharpNSByFile = map[string]csharpFileNS{}
	}
	r.csharpNSByFile[fileID] = ns
	r.csharpNSMu.Unlock()
	return ns
}

// csharpMetaStrings reads a []string-shaped meta value in either of the
// shapes a graph round-trip produces (in-memory []string, JSON []any).
func csharpMetaStrings(v any) []string {
	switch v := v.(type) {
	case []string:
		return v
	case []any:
		var out []string
		for _, u := range v {
			if s, ok := u.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// csharpGlobalUsingsFor returns the global-using namespaces visible to
// fileID. A `global using` is compilation-scoped, and the compilation
// unit is approximated by the nearest-ancestor directory owning a
// .csproj file node: globals declared anywhere in that unit apply to
// every file of the unit — including sibling subtrees — while a nested
// project with its own csproj is a separate unit that inherits nothing.
// Files outside any csproj directory degrade to the declaring file's
// directory subtree (better a narrower scope than a leak).
func (r *Resolver) csharpGlobalUsingsFor(fileID string) []string {
	idx, _, projDirs := r.csharpEnsureGlobalIndexes()
	return csharpUnitGlobalsFor(idx, projDirs, fileID)
}

// csharpGlobalStaticsFor is the `global using static` counterpart:
// the target class FQNs whose members — extension methods included —
// are in scope for every file of fileID's compilation unit.
func (r *Resolver) csharpGlobalStaticsFor(fileID string) []string {
	_, statics, projDirs := r.csharpEnsureGlobalIndexes()
	return csharpUnitGlobalsFor(statics, projDirs, fileID)
}

// csharpEnsureGlobalIndexes returns the derived unit indexes, seeding
// the sources (one workspace scan, first time only) and re-deriving
// (cheap) when a reconcile dropped them.
func (r *Resolver) csharpEnsureGlobalIndexes() (idx, statics map[string][]string, projDirs map[string]struct{}) {
	r.csharpNSMu.RLock()
	idx, statics, projDirs = r.csharpGlobalByDir, r.csharpGlobalStaticByDir, r.csharpProjDirs
	r.csharpNSMu.RUnlock()
	if idx == nil {
		r.csharpNSMu.Lock()
		if r.csharpGlobalByDir == nil {
			if !r.csharpVisSeeded {
				r.seedCSharpVisSourcesLocked()
			}
			r.deriveCSharpGlobalIndexLocked()
		}
		idx, statics, projDirs = r.csharpGlobalByDir, r.csharpGlobalStaticByDir, r.csharpProjDirs
		r.csharpNSMu.Unlock()
	}
	return idx, statics, projDirs
}

// csharpUnitGlobalsFor resolves one file's unit-scoped entries from a
// derived index: the nearest-csproj unit's list, else the walk-up
// collection for files outside any csproj.
func csharpUnitGlobalsFor(idx map[string][]string, projDirs map[string]struct{}, fileID string) []string {
	if len(idx) == 0 {
		return nil
	}
	// A caller owned by a csproj sees exactly its unit's globals —
	// walking further up would inherit an OUTER project's globals.
	if key := csharpNearestProjDir(projDirs, csharpDirOf(fileID)); key != "" {
		return idx[key]
	}
	var out []string
	dir := fileID
	for {
		i := csharpLastSep(dir)
		if i < 0 {
			out = append(out, idx["."]...)
			break
		}
		dir = dir[:i]
		out = append(out, idx[dir]...)
	}
	return out
}

// seedCSharpVisSourcesLocked is the ONE workspace scan: it records each
// global-using declaration and csproj location. After the seed the
// sources are maintained by scopedCSharpVisibilityInvalidate — the
// files a scoped resolve names re-read their stamps, and decl/proj
// existence is re-verified per entry — so no later scoped resolve pays
// the scan again (the per-save O(all files) rebuild the review flagged).
func (r *Resolver) seedCSharpVisSourcesLocked() {
	decls := map[string][]string{}
	staticDecls := map[string][]string{}
	projs := map[string]struct{}{}
	for n := range r.graph.NodesByKind(graph.KindFile) {
		if n == nil {
			continue
		}
		if strings.HasSuffix(strings.ToLower(n.ID), ".csproj") {
			projs[n.ID] = struct{}{}
			continue
		}
		if n.Meta == nil {
			continue
		}
		if globals := csharpMetaStrings(n.Meta["global_usings"]); len(globals) > 0 {
			decls[n.ID] = globals
		}
		if statics := csharpMetaStrings(n.Meta["global_using_static"]); len(statics) > 0 {
			staticDecls[n.ID] = statics
		}
	}
	r.csharpGlobalDecls = decls
	r.csharpGlobalStaticDecls = staticDecls
	r.csharpProjFiles = projs
	r.csharpVisSeeded = true
}

// deriveCSharpGlobalIndexLocked rebuilds the derived unit-key index
// from the maintained sources — O(|decls| + |projs|), no graph scan.
func (r *Resolver) deriveCSharpGlobalIndexLocked() {
	projSet := map[string]struct{}{}
	for f := range r.csharpProjFiles {
		projSet[csharpDirOf(f)] = struct{}{}
	}
	unitKey := func(f string) string {
		if key := csharpNearestProjDir(projSet, csharpDirOf(f)); key != "" {
			return key
		}
		return csharpDirOf(f)
	}
	built := map[string][]string{}
	for f, globals := range r.csharpGlobalDecls {
		key := unitKey(f)
		built[key] = append(built[key], globals...)
	}
	builtStatics := map[string][]string{}
	for f, statics := range r.csharpGlobalStaticDecls {
		key := unitKey(f)
		builtStatics[key] = append(builtStatics[key], statics...)
	}
	r.csharpGlobalByDir = built
	r.csharpGlobalStaticByDir = builtStatics
	r.csharpProjDirs = projSet
}

// scopedCSharpVisibilityInvalidate replaces the wholesale visibility
// clear at the scoped-resolve entry points. The graph-content memos
// (type lookup, ancestor closures) rebuild lazily and cheaply, so they
// still drop per entry; the global-using sources persist and only
// RECONCILE: each named file re-reads its stamps, and the (few) decl /
// csproj files are verified to still exist so a deletion through any
// mutation path invalidates without indexer wiring. Only an actual
// change drops the derived index and the per-file namespace sets
// (those bake the propagated globals in).
func (r *Resolver) scopedCSharpVisibilityInvalidate(files []string) {
	r.csharpNSMu.Lock()
	defer r.csharpNSMu.Unlock()
	r.csharpTypeNodesByName = nil
	r.csharpAncestorsByType = nil
	if !r.csharpVisSeeded {
		r.csharpGlobalByDir = nil
		r.csharpProjDirs = nil
		r.csharpNSByFile = nil
		return
	}
	changed := false
	for _, f := range files {
		if r.reconcileCSharpVisFileLocked(f) {
			changed = true
		}
	}
	for f := range r.csharpGlobalDecls {
		if r.graph.GetNode(f) == nil {
			delete(r.csharpGlobalDecls, f)
			changed = true
		}
	}
	for f := range r.csharpGlobalStaticDecls {
		if r.graph.GetNode(f) == nil {
			delete(r.csharpGlobalStaticDecls, f)
			changed = true
		}
	}
	for f := range r.csharpProjFiles {
		if r.graph.GetNode(f) == nil {
			delete(r.csharpProjFiles, f)
			changed = true
		}
	}
	if changed {
		r.csharpGlobalByDir = nil
		r.csharpGlobalStaticByDir = nil
		r.csharpProjDirs = nil
		r.csharpNSByFile = nil
		return
	}
	// Unchanged globals: only the named files' own using stamps could
	// have moved — drop just their per-file sets.
	for _, f := range files {
		delete(r.csharpNSByFile, f)
	}
}

// reconcileCSharpVisFileLocked folds one file's current global-using /
// csproj state into the maintained sources; true when it changed.
func (r *Resolver) reconcileCSharpVisFileLocked(fileID string) bool {
	if strings.HasSuffix(strings.ToLower(fileID), ".csproj") {
		_, had := r.csharpProjFiles[fileID]
		exists := r.graph.GetNode(fileID) != nil
		if had == exists {
			return false
		}
		if exists {
			r.csharpProjFiles[fileID] = struct{}{}
		} else {
			delete(r.csharpProjFiles, fileID)
		}
		return true
	}
	var globals, statics []string
	if n := r.graph.GetNode(fileID); n != nil && n.Meta != nil {
		globals = csharpMetaStrings(n.Meta["global_usings"])
		statics = csharpMetaStrings(n.Meta["global_using_static"])
	}
	changed := false
	if !csharpSameStringSet(r.csharpGlobalDecls[fileID], globals) {
		if len(globals) == 0 {
			delete(r.csharpGlobalDecls, fileID)
		} else {
			r.csharpGlobalDecls[fileID] = globals
		}
		changed = true
	}
	if !csharpSameStringSet(r.csharpGlobalStaticDecls[fileID], statics) {
		if len(statics) == 0 {
			delete(r.csharpGlobalStaticDecls, fileID)
		} else {
			r.csharpGlobalStaticDecls[fileID] = statics
		}
		changed = true
	}
	return changed
}

// csharpSameStringSet is an order-insensitive slice comparison — the
// stamp order is extraction-stable, but the decl map round-trips
// through map iteration.
func csharpSameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	as := append([]string(nil), a...)
	bs := append([]string(nil), b...)
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

// CSharpGlobalUsingDependents returns the C# files whose global-using
// visibility includes fileID's declarations — the same compilation-unit
// approximation csharpGlobalUsingsFor reads from (nearest-csproj unit,
// else the declaring file's directory subtree). A global-using edit in
// fileID must re-resolve exactly these: their extension binds were
// narrowed under the old visibility and nothing else touches them.
// fileID itself is excluded (its own save re-resolves it).
func (r *Resolver) CSharpGlobalUsingDependents(fileID string) []string {
	projSet := map[string]struct{}{}
	for n := range r.graph.NodesByKind(graph.KindFile) {
		if n != nil && strings.HasSuffix(strings.ToLower(n.ID), ".csproj") {
			projSet[csharpDirOf(n.ID)] = struct{}{}
		}
	}
	declKey := csharpNearestProjDir(projSet, csharpDirOf(fileID))
	var files []string
	for n := range r.graph.NodesByKind(graph.KindFile) {
		if n == nil || n.ID == fileID || !strings.HasSuffix(strings.ToLower(n.ID), ".cs") {
			continue
		}
		unit := csharpNearestProjDir(projSet, csharpDirOf(n.ID))
		if declKey != "" {
			if unit == declKey {
				files = append(files, n.ID)
			}
			continue
		}
		// No-csproj fallback: a caller outside any unit collects globals
		// from every ancestor directory, so the dependents are the files
		// whose ancestor chain reaches the declaring directory.
		if unit == "" && csharpDirChainContains(n.ID, csharpDirOf(fileID)) {
			files = append(files, n.ID)
		}
	}
	sort.Strings(files)
	return files
}

// csharpDirChainContains reports whether walking up from fileID's
// directory reaches dir — the reverse of csharpGlobalUsingsFor's
// caller-side ancestor walk.
func csharpDirChainContains(fileID, dir string) bool {
	d := fileID
	for {
		i := csharpLastSep(d)
		if i < 0 {
			return dir == "."
		}
		d = d[:i]
		if d == dir {
			return true
		}
	}
}

// csharpNearestProjDir walks from dir upward (dir included) and returns
// the deepest directory that owns a .csproj — "" when none does.
func csharpNearestProjDir(projDirs map[string]struct{}, dir string) string {
	if len(projDirs) == 0 {
		return ""
	}
	for {
		if _, ok := projDirs[dir]; ok {
			return dir
		}
		i := csharpLastSep(dir)
		if i < 0 {
			if _, ok := projDirs["."]; ok {
				return "."
			}
			return ""
		}
		dir = dir[:i]
	}
}

// csharpLastSep is the index of the last path separator. Windows stores
// join the repo prefix with "/" but keep OS-native "\" below it
// (the indexer's graphRelKey shape) — both must count, or every
// project collapses to the repo-root key and globals leak repo-wide.
func csharpLastSep(p string) int {
	return strings.LastIndexAny(p, `/\`)
}

// csharpDirOf is path.Dir over node IDs in either separator shape,
// with "." for a root-level file.
func csharpDirOf(p string) string {
	if i := csharpLastSep(p); i >= 0 {
		return p[:i]
	}
	return "."
}

// csharpNarrowEligible gates narrowing to type-shaped candidates in the
// dotnet family. Methods carry scope_ns too — letting one match would
// steal the bind from (or lose it for) the type the reference names.
func csharpNarrowEligible(c *graph.Node) bool {
	return c != nil && (c.Kind == graph.KindType || c.Kind == graph.KindInterface) &&
		sameLanguageFamily("csharp", c.Language)
}

// csharpNarrowByNamespace filters same-named C# type candidates to the
// namespaces the referencing file can see: written qualifier, then
// enclosing namespaces (deepest wins), then using directives. Same
// contract as phpNarrowByTargetFQN — narrowing only, never a loss.
func (r *Resolver) csharpNarrowByNamespace(e *graph.Edge, candidates []*graph.Node) []*graph.Node {
	if len(candidates) < 2 || !strings.HasSuffix(e.FilePath, ".cs") {
		return nil
	}

	// A written qualifier (Meta["target_fqn"]) is the strongest evidence.
	// C# resolves partial qualifiers against visible namespaces, so a
	// dot-boundary suffix match covers the full and partial forms alike.
	qualified := candidates
	if fqn, _ := e.Meta["target_fqn"].(string); strings.Contains(fqn, ".") {
		q := fqn[:strings.LastIndex(fqn, ".")]
		dotQ := "." + q
		var m []*graph.Node
		for _, c := range candidates {
			if !csharpNarrowEligible(c) {
				continue
			}
			ns, _ := c.Meta["scope_ns"].(string)
			if ns == q || strings.HasSuffix(ns, dotQ) {
				m = append(m, c)
			}
		}
		if len(m) == 1 {
			return m
		}
		if len(m) > 1 {
			qualified = m
		}
	}

	visible := r.csharpFileNamespaceSet(e.FilePath)
	if len(visible.enclosing) == 0 && len(visible.imported) == 0 {
		if len(qualified) < len(candidates) {
			return qualified
		}
		return nil
	}
	var enclosing, imported []*graph.Node
	deepest := 0
	for _, c := range qualified {
		if !csharpNarrowEligible(c) {
			continue
		}
		ns, _ := c.Meta["scope_ns"].(string)
		if ns == "" {
			continue
		}
		if _, ok := visible.enclosing[ns]; ok {
			switch {
			case len(ns) > deepest:
				enclosing, deepest = []*graph.Node{c}, len(ns)
			case len(ns) == deepest:
				enclosing = append(enclosing, c)
			}
			continue
		}
		if _, ok := visible.imported[ns]; ok {
			imported = append(imported, c)
		}
	}
	if len(enclosing) > 0 {
		return enclosing
	}
	if len(imported) > 0 {
		return imported
	}
	// The qualifier narrowed but no visible-namespace tier confirmed —
	// the written qualifier alone still beats a bare-name guess.
	if len(qualified) < len(candidates) {
		return qualified
	}
	return nil
}
