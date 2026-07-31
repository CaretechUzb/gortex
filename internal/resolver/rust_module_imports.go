package resolver

import (
	"path"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// resolveRustModuleImports binds Rust `use crate::…` / `use self::…` /
// `use super::…` import edges to the module file they reference, so a
// module-level dependency shows up in get_dependencies. The Rust extractor
// lowers `use crate::foo::bar;` to an `unresolved::import::crate/foo/bar` edge
// (the `::` separators rewritten to `/`); this pass anchors the path —
// `crate` at the crate src root, `self` at the caller's module directory,
// `super` one directory up per segment — and resolves it longest-prefix-first
// against `foo/bar.rs` | `foo/bar/mod.rs`, dropping trailing segments (the
// imported item or glob) until a module file matches. Refuses on ambiguity
// (both `foo.rs` and `foo/mod.rs` present). Bare paths (external crates or
// unanchored local modules) are left untouched.
//
// Runs inside ResolveRustScopeCalls (the SynthRustScope settle window),
// independent of the call-edge resolution there. Returns the number of import
// edges bound.
func resolveRustModuleImports(g graph.Store) int {
	var cands []*graph.Edge
	for e := range g.EdgesByKind(graph.EdgeImports) {
		if e == nil || !strings.HasPrefix(e.To, "unresolved::import::") {
			continue
		}
		if !strings.HasSuffix(e.From, ".rs") {
			continue
		}
		raw := strings.TrimPrefix(e.To, "unresolved::import::")
		i := strings.IndexByte(raw, '/')
		if i < 0 {
			continue
		}
		switch raw[:i] {
		case "crate", "self", "super":
			cands = append(cands, e)
		}
	}
	if len(cands) == 0 {
		return 0
	}

	fileIDs := make(map[string]struct{}, 1024)
	for _, id := range graph.NodeIDsForKinds(g, graph.KindFile) {
		if id != "" {
			fileIDs[id] = struct{}{}
		}
	}

	bound := 0
	var reindexBatch []graph.EdgeReindex
	for _, e := range cands {
		raw := strings.TrimPrefix(e.To, "unresolved::import::")
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		if _, exists := e.Meta["rust_use_path"]; !exists {
			e.Meta["rust_use_path"] = normalizeRustUsePath(raw)
		}
		target := resolveRustUseFile(e.From, strings.Split(raw, "/"), fileIDs)
		if target == "" || target == e.From {
			continue
		}
		oldTo := e.To
		e.To = target
		e.Origin = graph.OriginASTResolved
		if e.Meta == nil {
			e.Meta = map[string]any{}
		}
		e.Meta["resolved_via"] = "rust_module"
		reindexBatch = append(reindexBatch, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		bound++
	}
	if len(reindexBatch) > 0 {
		g.ReindexEdges(reindexBatch)
	}
	return bound
}

// resolveRustUseFile resolves a `/`-joined Rust use path (anchored at
// crate/self/super) to a module file ID, or "" when no unique file matches.
func resolveRustUseFile(srcFile string, segs []string, fileIDs map[string]struct{}) string {
	if len(segs) == 0 {
		return ""
	}
	dir := path.Dir(srcFile)
	rest := segs
	switch segs[0] {
	case "crate":
		dir = rustCrateRootDir(srcFile)
		rest = segs[1:]
	case "self":
		rest = segs[1:]
	case "super":
		for len(rest) > 0 && rest[0] == "super" {
			dir = path.Dir(dir)
			rest = rest[1:]
		}
	default:
		return ""
	}
	// Longest-prefix-first: try the full module path, then drop the trailing
	// segment (the imported item / glob) until a module file matches.
	for n := len(rest); n >= 1; n-- {
		stem := path.Join(append([]string{dir}, rest[:n]...)...)
		var matches []string
		for _, cand := range []string{stem + ".rs", stem + "/mod.rs"} {
			if _, ok := fileIDs[cand]; ok {
				matches = append(matches, cand)
			}
		}
		if len(matches) == 1 {
			return matches[0]
		}
		if len(matches) > 1 {
			return "" // ambiguous module file (foo.rs vs foo/mod.rs)
		}
	}
	// `use crate::*` / `use self::*` — the module's own directory file.
	if len(rest) == 0 {
		for _, cand := range []string{path.Clean(dir) + ".rs", path.Join(dir, "mod.rs")} {
			if _, ok := fileIDs[cand]; ok {
				return cand
			}
		}
	}
	return ""
}

// rustCrateRootDir returns the crate src root for a Rust file — the nearest
// ancestor directory named `src`, or the file's own directory for a flat crate.
func rustCrateRootDir(srcFile string) string {
	dir := path.Dir(srcFile)
	for d := dir; d != "." && d != "/" && d != ""; d = path.Dir(d) {
		if path.Base(d) == "src" {
			return d
		}
	}
	return dir
}
