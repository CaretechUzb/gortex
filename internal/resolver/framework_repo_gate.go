package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/frameworkgate"
	"github.com/zzet/gortex/internal/graph"
)

// Per-repository enforcement of index.frameworks.allow.
//
// The workspace-wide synthesis pass writes into one shared graph, so the
// question "may this pass run" and the question "may this pass write HERE"
// have different answers. frameworkgate.Union answers the first
// permissively on purpose: a repository that narrowed its allow-list must
// not strip a sibling's edges, and the sibling never opted out. That
// leaves the narrowing repository unprotected, though — the pass it
// excluded still runs, and its edges still land on that repository's
// nodes.
//
// This gate closes that half. The union decides whether a pass executes
// at all; the gate decides, per edge, whether the pass may write into the
// repository that owns the edge's SOURCE node. A repository with no
// allow-list has an unset Set, which allows everything, so the default
// costs nothing and cannot blind a graph.
//
// The source side is the deciding end deliberately. A dispatch edge is
// attributed to the caller: `addons/x.py -> odoo/y.py` is work the addons
// repository asked for, and addons' allow-list governs it. Gating on the
// target instead would let one repository's config silently delete
// another repository's outgoing edges.

// frameworkRepoPrefix returns the repository prefix that owns a node ID.
//
// Every indexed node ID begins with `<repo_prefix>/` — the invariant
// asserted by MultiIndexer's node-ID test. Synthetic IDs that predate a
// repository (`unresolved::…` placeholders, builtin stubs) carry no
// prefix and yield "", which the caller reads as "unknown, fail open".
func frameworkRepoPrefix(id string) string {
	if i := strings.IndexByte(id, '/'); i > 0 {
		// A "::" ahead of the first "/" means the ID is synthetic
		// (`unresolved::odoo::xmlid::sale/order`), not repo-owned.
		if j := strings.Index(id, "::"); j >= 0 && j < i {
			return ""
		}
		return id[:i]
	}
	return ""
}

// frameworkRepoGate answers whether one named pass may write into a given
// repository. A nil gate admits everything.
type frameworkRepoGate struct {
	byRepo map[string]frameworkgate.Set
}

// newFrameworkRepoGate returns a gate for byRepo, or nil when no
// repository narrows anything — the common case, which must stay free.
func newFrameworkRepoGate(byRepo map[string]frameworkgate.Set) *frameworkRepoGate {
	narrowing := false
	for _, s := range byRepo {
		if s.Configured() {
			narrowing = true
			break
		}
	}
	if !narrowing {
		return nil
	}
	return &frameworkRepoGate{byRepo: byRepo}
}

// admitsPass reports whether any repository excludes this pass. When none
// does, the caller can skip wrapping the store for that pass entirely.
func (gate *frameworkRepoGate) excludesPass(name string) bool {
	if gate == nil {
		return false
	}
	for _, s := range gate.byRepo {
		if !s.Allows(name) {
			return true
		}
	}
	return false
}

// admits reports whether the repository owning fromID admits the pass.
// An unknown prefix fails OPEN: a synthetic or cross-repo source we
// cannot attribute must not lose its edge to a config it never declared.
func (gate *frameworkRepoGate) admits(fromID, pass string) bool {
	if gate == nil {
		return true
	}
	prefix := frameworkRepoPrefix(fromID)
	if prefix == "" {
		return true
	}
	set, ok := gate.byRepo[prefix]
	if !ok {
		return true
	}
	return set.Allows(pass)
}

// frameworkRepoGateStore drops the edge writes one pass is not allowed to
// make in the repository owning the edge's source node. Reads are the
// embedded store's, untouched: a gated pass still sees the whole graph,
// it just cannot record its conclusions where it was not invited.
//
// EVERY edge-write path has to be covered, not just AddEdge. The registry
// holds two shapes of pass: synthesizers, which ADD edges, and resolvers
// (value-ref, react-resolve, fastapi-resolve, celery-dispatch), which
// REWRITE an already-persisted unresolved edge by mutating its To and
// calling ReindexEdge(s). Gating only the add path leaves every resolver
// ungated — measured on a live workspace, it let 6,496 value-ref and
// 1,190 react-resolve edges bind inside repositories that had excluded
// those passes, while the report showed repo_gated:0 for them because
// nothing was ever offered to the gate.
type frameworkRepoGateStore struct {
	graph.Store
	gate    *frameworkRepoGate
	pass    string
	dropped int
}

// refusesEdge reports whether this pass may not write the given edge.
// Both endpoints of a moving identity are checked: a pass must neither
// touch a row already owned by an excluding repository (oldFrom) nor
// move one into it (e.From).
func (v *frameworkRepoGateStore) refusesEdge(e *graph.Edge, oldFrom string) bool {
	if e == nil {
		return false
	}
	if !v.gate.admits(e.From, v.pass) {
		return true
	}
	return oldFrom != "" && !v.gate.admits(oldFrom, v.pass)
}

// newFrameworkRepoGateStore wraps store for one pass, or returns store
// unchanged when nothing can be dropped. Returning the bare store keeps
// the unconfigured hot path free of an extra interface hop per edge.
func newFrameworkRepoGateStore(store graph.Store, gate *frameworkRepoGate, pass string) graph.Store {
	if store == nil || !gate.excludesPass(pass) {
		return store
	}
	return &frameworkRepoGateStore{Store: store, gate: gate, pass: pass}
}

// droppedFrameworkRepoEdges reports how many edges the gate refused, for
// the run report. A store that was never wrapped dropped nothing.
func droppedFrameworkRepoEdges(store graph.Store) int {
	if gated, ok := store.(*frameworkRepoGateStore); ok {
		return gated.dropped
	}
	return 0
}

func (v *frameworkRepoGateStore) AddEdge(e *graph.Edge) {
	if e != nil && !v.gate.admits(e.From, v.pass) {
		v.dropped++
		return
	}
	v.Store.AddEdge(e)
}

// AddBatch filters the edge half of a batch. Nodes pass through: a pass
// that mints a node is describing something it found, not asserting an
// edge into a repository that excluded it.
func (v *frameworkRepoGateStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	kept := edges[:0:0]
	for _, e := range edges {
		if e != nil && !v.gate.admits(e.From, v.pass) {
			v.dropped++
			continue
		}
		kept = append(kept, e)
	}
	v.Store.AddBatch(nodes, kept)
}

// ReindexEdge is the resolvers' path: the pass has already mutated the
// edge's To in memory and is asking for it to be persisted. Refusing the
// persist leaves the stored row unresolved, which is exactly what a
// repository that excluded the pass asked for.
func (v *frameworkRepoGateStore) ReindexEdge(e *graph.Edge, oldTo string) {
	if v.refusesEdge(e, "") {
		v.dropped++
		return
	}
	v.Store.ReindexEdge(e, oldTo)
}

func (v *frameworkRepoGateStore) ReindexEdges(batch []graph.EdgeReindex) {
	kept := batch[:0:0]
	for _, r := range batch {
		if v.refusesEdge(r.Edge, r.OldFrom) {
			v.dropped++
			continue
		}
		kept = append(kept, r)
	}
	v.Store.ReindexEdges(kept)
}

func (v *frameworkRepoGateStore) SetEdgeProvenance(e *graph.Edge, newOrigin string) bool {
	if v.refusesEdge(e, "") {
		v.dropped++
		return false
	}
	return v.Store.SetEdgeProvenance(e, newOrigin)
}

func (v *frameworkRepoGateStore) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) int {
	kept := batch[:0:0]
	for _, u := range batch {
		if v.refusesEdge(u.Edge, "") {
			v.dropped++
			continue
		}
		kept = append(kept, u)
	}
	return v.Store.SetEdgeProvenanceBatch(kept)
}

// RemoveEdge is gated for the same reason as the writes: a pass a
// repository excluded has no standing to delete that repository's edges
// either. A pass that rewrites by remove-then-add is blocked on both
// halves, so the row is left exactly as it was.
func (v *frameworkRepoGateStore) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	if !v.gate.admits(from, v.pass) {
		v.dropped++
		return false
	}
	return v.Store.RemoveEdge(from, to, kind)
}
