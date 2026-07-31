package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// scopeNode is the per-binding payload of the owner-keyed scope
// index built by bindBareNameScopeRefs. Kept as a named struct so
// the bind helpers can share the same signature.
type scopeNode struct {
	id        string
	name      string
	startLine int
	kind      graph.NodeKind
}

// bindBareNameScopeRefs rewrites `unresolved::<bareName>` edges whose
// source is inside a function scope (or IS a function) onto the
// matching KindLocal / KindParam node that the enclosing function
// declares. Pre-#77 there was nothing to bind to — locals were
// edge-endpoint-only — so the resolver always fell through to
// `unresolved::*`. With #77's KindLocal materialisation the scope is
// now first-class and we can do the bind.
//
// Two precedence rules govern the choice when more than one candidate
// matches the name:
//
//  1. KindLocal beats KindParam — Go shadowing semantics, a local
//     declared with the same name as a parameter takes over from its
//     declaration line onwards.
//  2. Among KindLocal candidates the most recently declared one before
//     the reference line wins (the standard "last shadow in scope"
//     rule). The edge's Line field is the reference site; we filter
//     candidates to StartLine <= reference line and pick the maximum
//     StartLine.
//
// Ambiguous cases that don't resolve to one winner (e.g. two locals
// with the same Name on the same StartLine, or no candidate before
// the reference line) are left untouched so the downstream `unresolved`
// audit can still surface them.
//
// Scope today is Go-only — TypeScript / Python don't materialise
// locals yet, so their unresolved bare-name edges have no candidate
// to bind to. The pass naturally degenerates to a no-op for those
// languages because the candidate index will be empty for their
// owners.
func (r *Resolver) bindBareNameScopeRefs() {
	// Index every KindLocal / KindParam by enclosing-function ID. Done
	// once up front so the per-edge bind is an O(matching-name) walk
	// rather than a graph-wide FindNodesByName.
	owned := map[string][]scopeNode{}
	for n := range r.graph.NodesByKind(graph.KindLocal) {
		owner := enclosingFunctionForBinding(n.ID)
		if owner == "" {
			continue
		}
		owned[owner] = append(owned[owner], scopeNode{
			id: n.ID, name: n.Name, startLine: n.StartLine, kind: graph.KindLocal,
		})
	}
	for n := range r.graph.NodesByKind(graph.KindParam) {
		owner := enclosingFunctionForBinding(n.ID)
		if owner == "" {
			continue
		}
		owned[owner] = append(owned[owner], scopeNode{
			id: n.ID, name: n.Name, startLine: n.StartLine, kind: graph.KindParam,
		})
	}
	if len(owned) == 0 {
		return
	}

	// EdgeArgOf and EdgeValueFlow carry the same shape — `unresolved::<name>`
	// is the dataflow source/target the parser couldn't bind.
	r.reindexAttributionEdgesBatched(
		[]graph.EdgeKind{
			graph.EdgeReads,
			graph.EdgeReferences,
			graph.EdgeArgOf,
			graph.EdgeValueFlow,
		},
		func(edge *graph.Edge) string {
			return r.tryBindBareName(edge, owned)
		},
	)
}

// bindBareNameScopeRefsForFile is the single-file scope of
// bindBareNameScopeRefs. A bare-name reference binds to a KindLocal /
// KindParam declared by its OWN enclosing function, and that function —
// with all of its locals and params — lives entirely in the edited file.
// So the scope index is built from the file's own KindLocal / KindParam
// nodes and only the file's outgoing Read / Reference / ArgOf / ValueFlow
// edges are considered. This produces the same binds as the whole-graph
// sweep for a per-save resolve without scanning every KindLocal in the
// graph (the single largest node kind).
func (r *Resolver) bindBareNameScopeRefsForFile(filePath string) {
	owned := map[string][]scopeNode{}
	for _, n := range r.incrementalFileNodes(filePath) {
		if n.Kind != graph.KindLocal && n.Kind != graph.KindParam {
			continue
		}
		owner := enclosingFunctionForBinding(n.ID)
		if owner == "" {
			continue
		}
		owned[owner] = append(owned[owner], scopeNode{
			id: n.ID, name: n.Name, startLine: n.StartLine, kind: n.Kind,
		})
	}
	if len(owned) == 0 {
		return
	}

	collector := newAttributionReindexCollector(r)
	for _, edge := range r.fileOutEdges(filePath) {
		switch edge.Kind {
		case graph.EdgeReads, graph.EdgeReferences, graph.EdgeArgOf, graph.EdgeValueFlow:
			collector.add(edge, r.tryBindBareName(edge, owned))
		}
	}
	collector.flush()
}

// tryBindBareName tries to rewrite e.To from `unresolved::<name>` to a
// matching in-scope KindLocal/KindParam ID. Returns the original To
// value when a rewrite happened (caller batches it for ReindexEdges)
// or "" when the edge was left alone.
func (r *Resolver) tryBindBareName(e *graph.Edge, owned map[string][]scopeNode) string {
	if e == nil || !graph.IsUnresolvedTarget(e.To) {
		return ""
	}
	name := graph.UnresolvedName(e.To)
	if name == "" || strings.ContainsAny(name, ".*:#") {
		// Not a bare identifier — leave to other passes (qualified
		// names, *.method, etc.).
		return ""
	}
	ownerID := enclosingFunctionForBinding(e.From)
	if ownerID == "" {
		return ""
	}
	candidates := owned[ownerID]
	if len(candidates) == 0 {
		return ""
	}
	chosen := pickInScopeBinding(candidates, name, e.Line)
	if chosen == "" || chosen == e.To {
		return ""
	}
	oldTo := e.To
	e.To = chosen
	return oldTo
}

// pickInScopeBinding implements the precedence rules:
//   - prefer KindLocal over KindParam (Go shadowing),
//   - among KindLocal, pick the latest StartLine that's still <= refLine,
//   - if multiple candidates match the same maximum StartLine, return ""
//     (ambiguous — leave the edge unresolved so the audit surfaces it).
//
// owned is the per-owner scope-node slice; name is the bare identifier
// from the edge target; refLine is the edge's line (the reference
// site). Returns the chosen ID, or "" when no unambiguous winner.
func pickInScopeBinding(owned []scopeNode, name string, refLine int) string {
	var bestLocal struct {
		id   string
		line int
		dups int
	}
	var paramID string
	for _, c := range owned {
		if c.name != name {
			continue
		}
		if c.kind == graph.KindLocal {
			if refLine > 0 && c.startLine > refLine {
				// Declared after the reference — can't be bound here.
				continue
			}
			switch {
			case c.startLine > bestLocal.line:
				bestLocal.id = c.id
				bestLocal.line = c.startLine
				bestLocal.dups = 0
			case c.startLine == bestLocal.line && c.id != bestLocal.id:
				bestLocal.dups++
			}
		} else if c.kind == graph.KindParam {
			if paramID != "" && paramID != c.id {
				// Two params with the same name in the same function
				// shouldn't happen but defensive — abstain.
				paramID = ""
			} else {
				paramID = c.id
			}
		}
	}
	if bestLocal.id != "" && bestLocal.dups == 0 {
		return bestLocal.id
	}
	return paramID
}

// enclosingFunctionForBinding strips the per-binding suffix added by
// the Go extractor (`#local:`, `#param:`, `#closure`, `#tparam:`) to
// recover the owner function/method ID. If `id` has no suffix it's
// returned unchanged — the caller is already a function/method node
// directly (the per-edge From is the function itself for things like
// the `external::foo` import edge inside `func Foo()`).
func enclosingFunctionForBinding(id string) string {
	if i := strings.Index(id, "#"); i > 0 {
		return id[:i]
	}
	return id
}
