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
//
// The same wrapper also enforces the repository-checkout invariant: no
// framework pass may write an edge between two git worktrees of one
// repository (see graph/checkout_groups.go). That is not a config
// question — the two prefixes are one body of code, so the edge carries
// no information whichever repository allowed the pass — but it needs
// exactly the same coverage of exactly the same write paths, and every
// registered synthesizer and claiming resolver already funnels through
// here. The two refusals are counted apart so the run report never
// attributes a checkout drop to an allow-list.
type frameworkRepoGateStore struct {
	graph.Store
	gate *frameworkRepoGate
	pass string
	// siblingSrc is the store the checkout grouping is published on. It is
	// NOT always the embedded Store: a scoped pass writes through a
	// pass-local seed store that knows nothing about repository topology.
	siblingSrc      any
	dropped         int
	droppedSiblings int
}

// CheckoutGroup and HasCheckoutGroups republish the checkout grouping that
// siblingSrc carries.
//
// Without them the wrapper is opaque to it. graph.CheckoutGrouped is not
// part of graph.Store, so embedding the interface promotes nothing, and
// every pass that asks its own store "does this workspace hold sibling
// checkouts?" was told no — including the Odoo model binder, whose
// odooSiblingCache then short-circuited to the identity and filtered
// nothing. Measured on a workspace with three checkouts of one repository:
// the binder staged 229,249 cross-checkout candidates and this wrapper
// refused every one of them, one edge at a time, inside a pass that took
// 183s. The graph was correct — refuses() reads siblingSrc directly, so
// the invariant held — but the work was done twice and thrown away once.
//
// Forwarding moves the decision back to where the candidate is built. The
// gate below stays as the backstop for passes that do not ask.
func (v *frameworkRepoGateStore) CheckoutGroup(repoPrefix string) string {
	grouped, ok := v.siblingSrc.(graph.CheckoutGrouped)
	if !ok {
		return ""
	}
	return grouped.CheckoutGroup(repoPrefix)
}

func (v *frameworkRepoGateStore) HasCheckoutGroups() bool {
	grouped, ok := v.siblingSrc.(graph.CheckoutGrouped)
	return ok && grouped.HasCheckoutGroups()
}

// refuses reports whether this pass may not write the edge, recording
// which of the two rules refused it.
//
// The checkout rule is checked first and separately: it is an invariant
// about the graph, not a permission the pass could have been granted, so
// counting it as an allow-list drop would misreport both numbers.
func (v *frameworkRepoGateStore) refuses(e *graph.Edge, oldFrom string) bool {
	if e == nil {
		return false
	}
	if graph.SiblingCheckoutIDs(v.siblingSrc, e.From, e.To) {
		v.droppedSiblings++
		return true
	}
	if v.refusesEdge(e, oldFrom) {
		v.dropped++
		return true
	}
	return false
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
// siblingSrc is where the checkout grouping lives — the workspace graph,
// which for a scoped pass is not the store being written through.
func newFrameworkRepoGateStore(store graph.Store, gate *frameworkRepoGate, pass string, siblingSrc any) graph.Store {
	if store == nil {
		return store
	}
	if !gate.excludesPass(pass) && !graph.HasSiblingCheckouts(siblingSrc) {
		return store
	}
	return &frameworkRepoGateStore{Store: store, gate: gate, pass: pass, siblingSrc: siblingSrc}
}

// droppedFrameworkRepoEdges reports how many edges the gate refused, for
// the run report. A store that was never wrapped dropped nothing.
func droppedFrameworkRepoEdges(store graph.Store) int {
	if gated, ok := store.(*frameworkRepoGateStore); ok {
		return gated.dropped
	}
	return 0
}

// droppedSiblingCheckoutEdges reports how many edges were refused for
// spanning two checkouts of one repository. Reported apart from the
// allow-list drops so neither number explains the other away.
func droppedSiblingCheckoutEdges(store graph.Store) int {
	if gated, ok := store.(*frameworkRepoGateStore); ok {
		return gated.droppedSiblings
	}
	return 0
}

func (v *frameworkRepoGateStore) AddEdge(e *graph.Edge) {
	if v.refuses(e, "") {
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
		if v.refuses(e, "") {
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
	if v.refuses(e, "") {
		return
	}
	v.Store.ReindexEdge(e, oldTo)
}

func (v *frameworkRepoGateStore) ReindexEdges(batch []graph.EdgeReindex) {
	kept := batch[:0:0]
	for _, r := range batch {
		if v.refuses(r.Edge, r.OldFrom) {
			continue
		}
		kept = append(kept, r)
	}
	v.Store.ReindexEdges(kept)
}

func (v *frameworkRepoGateStore) SetEdgeProvenance(e *graph.Edge, newOrigin string) bool {
	if v.refuses(e, "") {
		return false
	}
	return v.Store.SetEdgeProvenance(e, newOrigin)
}

func (v *frameworkRepoGateStore) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) int {
	kept := batch[:0:0]
	for _, u := range batch {
		if v.refuses(u.Edge, "") {
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
