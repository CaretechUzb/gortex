package mcp

import (
	"context"
	"sort"

	"github.com/zzet/gortex/internal/astquery"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// This file owns enclosing-scope resolution shared across the search
// and AST tools: the per-file line->symbol index (fileSymbolIndex)
// and the enclosing-owner derivation (enclosingName). search_ast,
// search_text, search_symbols, and the analyze-* detectors all need
// to answer "which symbol contains this?" -- they share this code so
// the answer stays consistent.

// buildFileSymbolIndex returns one fileSymbolIndex per Target's graph
// path. Building all indexes up-front (instead of lazily on first
// match) is fine because the cost is one graph pass per file's
// symbol list, and the alternative — locking inside the worker pool
// hot path — is worse for parallel runs.
func (s *Server) buildFileSymbolIndex(targets []astquery.Target) map[string]*fileSymbolIndex {
	if s.graph == nil {
		return nil
	}
	wanted := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		wanted[t.GraphPath] = struct{}{}
	}
	out := make(map[string]*fileSymbolIndex, len(wanted))
	for _, n := range s.graph.AllNodes() {
		if _, ok := wanted[n.FilePath]; !ok {
			continue
		}
		// Functions, methods, closures, and macros are meaningful
		// "enclosing scope" candidates. Macro nodes are declaration-backed and
		// already carry exact source ranges; token-tree contents are never parsed
		// as declarations here. KindType (struct/class) is included too so a
		// class-body match still gets a symbol identity.
		switch n.Kind {
		case graph.KindFunction, graph.KindMethod, graph.KindClosure, graph.KindMacro,
			graph.KindType, graph.KindInterface:
			idx := out[n.FilePath]
			if idx == nil {
				idx = &fileSymbolIndex{}
				out[n.FilePath] = idx
			}
			idx.add(n)
		}
	}
	for _, idx := range out {
		idx.finalise()
	}
	return out
}

// fileSymbolIndex is the per-file lookup used by the SymbolLookup
// closure. We keep symbols sorted by [StartLine, EndLine] descending
// width so `find` returns the deepest enclosing scope (a closure
// inside a method beats the method itself).
type fileSymbolIndex struct {
	syms      []*graph.Node
	fileNode  *graph.Node
	saturated bool
}

func (i *fileSymbolIndex) add(n *graph.Node) { i.syms = append(i.syms, n) }

func (i *fileSymbolIndex) finalise() {
	sort.Slice(i.syms, func(a, b int) bool {
		if i.syms[a].StartLine != i.syms[b].StartLine {
			return i.syms[a].StartLine < i.syms[b].StartLine
		}
		// For nodes at the same start line, narrowest-first so the deepest
		// scope wins. Equal line ranges are not distinguishable without byte
		// positions, so canonical identity supplies a deterministic final tie.
		spanA := i.syms[a].EndLine - i.syms[a].StartLine
		spanB := i.syms[b].EndLine - i.syms[b].StartLine
		if spanA != spanB {
			return spanA < spanB
		}
		return i.syms[a].ID < i.syms[b].ID
	})
}

// smallestEnclosing returns the narrowest symbol whose [StartLine,
// EndLine] range covers `line`, or nil when no symbol does. Lines are
// 1-based; graph nodes store the same convention. syms is sorted by
// StartLine ascending, so the scan can stop once StartLine passes line.
func (i *fileSymbolIndex) smallestEnclosing(line int) *graph.Node {
	if i == nil || i.saturated {
		return nil
	}
	var best *graph.Node
	bestSpan := int(^uint(0) >> 1)
	for _, n := range i.syms {
		if n.StartLine > line {
			break
		}
		if n.EndLine < line {
			continue
		}
		span := n.EndLine - n.StartLine
		if best == nil || span < bestSpan {
			best = n
			bestSpan = span
		}
	}
	return best
}

// find returns (symbol_id, name) for the smallest enclosing symbol
// whose [StartLine, EndLine] range covers `line`.
func (i *fileSymbolIndex) find(line int) (string, string) {
	best := i.smallestEnclosing(line)
	if best == nil {
		return "", ""
	}
	return best.ID, best.Name
}

// enclosingForRange returns the symbols that enclose any line in the
// inclusive [start, end] range, choosing the smallest enclosing symbol
// at each covered line — so a range inside one function yields that
// function, while a range spanning two functions yields both. Results
// are deduplicated by node ID and returned in first-seen (top-down)
// order. A degenerate range (end < start) collapses to the single
// start line.
func (i *fileSymbolIndex) enclosingForRange(start, end int) []*graph.Node {
	if i == nil {
		return nil
	}
	if end < start {
		end = start
	}
	seen := make(map[string]struct{})
	var out []*graph.Node
	for line := start; line <= end; line++ {
		best := i.smallestEnclosing(line)
		if best == nil {
			continue
		}
		if _, ok := seen[best.ID]; ok {
			continue
		}
		seen[best.ID] = struct{}{}
		out = append(out, best)
	}
	return out
}

// enclosingName derives the enclosing owner of a node -- the symbol
// the node is declared *inside* -- and returns its (id, name).
//
//   - For a method, the owner is its receiver type, recovered from
//     the EdgeMemberOf edge, or failing that from the node ID prefix
//     (the ID convention is "<file>::<Owner>.<method>").
//   - For a field, enum member, closure, or nested function, the
//     owner is whatever EdgeMemberOf points at -- the struct, enum,
//     or enclosing function.
//   - For everything else (a top-level function, type, package-level
//     variable) there is no enclosing owner; both return values are
//     empty.
//
// A nil node or reader yields two empty strings.
func enclosingName(n *graph.Node, g graph.Reader) (id, name string) {
	if n == nil {
		return "", ""
	}
	switch n.Kind {
	case graph.KindMethod, graph.KindField, graph.KindEnumMember,
		graph.KindClosure:
		// These kinds are always declared inside an owner.
	case graph.KindFunction:
		// A function is enclosed only when it is nested (a function
		// literal assigned inside another function); the EdgeMemberOf
		// lookup below detects that. A top-level function has none.
	default:
		return "", ""
	}

	// Primary path: the EdgeMemberOf edge records the structural
	// owner directly.
	if g != nil {
		for _, e := range g.GetOutEdges(n.ID) {
			if e.Kind != graph.EdgeMemberOf {
				continue
			}
			if owner := g.GetNode(e.To); owner != nil {
				return owner.ID, owner.Name
			}
			// The edge target may not be resolvable to a node (an
			// unresolved owner); still surface the ID.
			return e.To, graph.EnclosingShortName(e.To)
		}
	}

	// Fallback: derive the owner from the ID convention. This
	// covers method / field / enum-member / closure even when no
	// EdgeMemberOf edge was materialised.
	if ownerID, ownerName := graph.EnclosingFromID(n.ID, n.Kind); ownerName != "" {
		return ownerID, ownerName
	}
	return "", ""
}

const (
	localizationFileNodeLimit    = 1_024
	localizationFileRequestLimit = 4_096
)

var localizationFileIndexKinds = []graph.NodeKind{
	graph.KindFile, graph.KindFunction, graph.KindMethod, graph.KindClosure,
	graph.KindMacro, graph.KindType, graph.KindInterface,
}

// buildFileSymbolIndexForPaths builds one bounded fileSymbolIndex per file
// path. Compatibility callers without a request scope still use the same typed
// bounded projection; there is deliberately no GetFileNodes fallback.
func (s *Server) buildFileSymbolIndexForPaths(paths map[string]struct{}) map[string]*fileSymbolIndex {
	return s.buildFileSymbolIndexForPathsContext(context.Background(), paths)
}

func (s *Server) buildFileSymbolIndexForPathsContext(ctx context.Context, paths map[string]struct{}) map[string]*fileSymbolIndex {
	return s.buildFileSymbolIndexForPathsScopedContext(ctx, paths, query.QueryOptions{})
}

func (s *Server) buildFileSymbolIndexForPathsScopedContext(
	ctx context.Context,
	paths map[string]struct{},
	opts query.QueryOptions,
) map[string]*fileSymbolIndex {
	ordered := make([]string, 0, len(paths))
	for path := range paths {
		ordered = append(ordered, path)
	}
	sort.Strings(ordered)
	return s.buildFileSymbolIndexForOrderedPathsScopedContext(ctx, ordered, opts)
}

func (s *Server) buildFileSymbolIndexForOrderedPathsContext(ctx context.Context, paths []string) map[string]*fileSymbolIndex {
	return s.buildFileSymbolIndexForOrderedPathsScopedContext(ctx, paths, query.QueryOptions{})
}

// buildFileSymbolIndexForOrderedPathsScopedContext preserves caller priority,
// applies request/session scope before each storage cap, and shares one strict
// node budget across the request. Saturated and unavailable paths retain an
// explicit marker so an exact lookup cannot fall through to a compatibility
// alias and misattribute an omitted narrower declaration.
func (s *Server) buildFileSymbolIndexForOrderedPathsScopedContext(
	ctx context.Context,
	paths []string,
	opts query.QueryOptions,
) map[string]*fileSymbolIndex {
	if len(paths) == 0 {
		return nil
	}
	reader := s.readerFor(ctx)
	bounded, ok := reader.(graph.BoundedFileNodeReader)
	if reader == nil || !ok || ctx.Err() != nil {
		return saturatedFileSymbolIndexes(paths)
	}

	scope := s.localizationNodeScopeWithTests(ctx, opts, false, localizationFileIndexKinds...)
	out := make(map[string]*fileSymbolIndex, len(paths))
	remaining := localizationFileRequestLimit
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, duplicate := out[path]; duplicate {
			continue
		}
		if ctx.Err() != nil {
			return saturateMissingFileSymbolIndexes(out, paths)
		}
		if remaining <= 0 {
			out[path] = &fileSymbolIndex{saturated: true}
			continue
		}
		limit := localizationFileNodeLimit
		if remaining < limit {
			limit = remaining
		}
		page, err := bounded.FindFileNodesBounded(ctx, path, scope, limit)
		if err != nil || ctx.Err() != nil {
			return saturateMissingFileSymbolIndexes(out, paths)
		}
		consumed := page.Total
		if consumed > remaining {
			consumed = remaining
		}
		remaining -= consumed
		if page.Truncated {
			out[path] = &fileSymbolIndex{saturated: true}
			continue
		}

		idx := &fileSymbolIndex{}
		for _, node := range page.Nodes {
			if node == nil {
				continue
			}
			if node.Kind == graph.KindFile {
				if idx.fileNode == nil || node.ID < idx.fileNode.ID {
					idx.fileNode = node
				}
				continue
			}
			idx.add(node)
		}
		if len(idx.syms) == 0 && idx.fileNode == nil {
			continue
		}
		idx.finalise()
		out[path] = idx
		for _, node := range page.Nodes {
			if node != nil && node.FilePath != "" {
				if _, exists := out[node.FilePath]; !exists {
					out[node.FilePath] = idx
				}
			}
		}
	}
	return out
}

func saturatedFileSymbolIndexes(paths []string) map[string]*fileSymbolIndex {
	return saturateMissingFileSymbolIndexes(make(map[string]*fileSymbolIndex, len(paths)), paths)
}

func saturateMissingFileSymbolIndexes(
	out map[string]*fileSymbolIndex,
	paths []string,
) map[string]*fileSymbolIndex {
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, complete := out[path]; !complete {
			out[path] = &fileSymbolIndex{saturated: true}
		}
	}
	return out
}
