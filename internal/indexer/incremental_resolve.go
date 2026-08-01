package indexer

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Incremental resolution reuse for the single-file edit path.
//
// A normal save evicts the whole file, re-parses it (every edge comes back
// unresolved), and re-resolves all of them — on a large file that means
// fetching tens of thousands of candidate nodes and scoring thousands of
// edges, even for a one-line change. But only the edited region's references
// actually changed; the rest resolve to exactly what they did before.
//
// So before eviction we snapshot the file's already-resolved outgoing edges,
// keyed by their *source-side shape* (origin symbol, kind, receiver type,
// referenced name) — independent of line number and of the resolved target.
// After the re-parse, any freshly extracted edge whose shape matches a unique
// captured resolution is pre-pointed at that target before it enters the
// graph, so the resolver skips it and only touches genuinely-new edges.
//
// Correctness is conservative by construction: a shape that resolved to two
// different targets (the key cannot tell them apart) is dropped and re-resolved
// from scratch, and a captured target that no longer exists is ignored. The
// reuse therefore can only ever reproduce a prior resolution or fall back to
// the full resolver — never invent a wrong target.

type reuseKey struct {
	from string
	kind graph.EdgeKind
	recv string // receiver type for method calls; "" otherwise
	name string // referenced identifier
}

type reuseVal struct {
	to         string
	confidence float64
	confLabel  string
	origin     string
	tier       string
}

// captureIncrementalState snapshots, in one walk of the file's outgoing edges:
//   - reuse: resolved in-repo edges keyed by source shape, so an unchanged
//     reference recovers its exact target (ambiguous keys poisoned to nil).
//   - priorUnresolved: shallow copies of the edges that were still unresolved,
//     so the resolver's forward pass can skip re-trying them (they point at
//     stubs the incoming pass binds when a target appears).
//
// external:: edges are neither reused nor skipped here: they are not
// IsUnresolvedTarget, so the resolver already leaves them alone.
func captureIncrementalState(g graph.Store, graphPath string) (reuse map[reuseKey]*reuseVal, priorUnresolved []*graph.Edge) {
	reuse = map[reuseKey]*reuseVal{}
	nodes := g.GetFileNodes(graphPath)
	ids := make([]string, 0, len(nodes))
	for _, n := range nodes {
		if n != nil {
			ids = append(ids, n.ID)
		}
	}
	byNode := graph.OutEdgesForNodes(g, ids)
	// Resolve every reusable target in one bounded lookup. The old point lookup
	// below made this path one SQLite query per resolved reference in the edited
	// file, which dominates warm/partial saves for generated or call-heavy code.
	targetIDSet := make(map[string]struct{})
	for _, n := range nodes {
		if n == nil {
			continue
		}
		for _, e := range byNode[n.ID] {
			if reusableResolvedEdge(e) {
				targetIDSet[e.To] = struct{}{}
			}
		}
	}
	targetIDs := make([]string, 0, len(targetIDSet))
	for id := range targetIDSet {
		targetIDs = append(targetIDs, id)
	}
	var targets map[string]*graph.Node
	if len(targetIDs) > 0 {
		targets = g.GetNodesByIDs(targetIDs)
	}
	for _, n := range nodes {
		if n == nil {
			continue
		}
		for _, e := range byNode[n.ID] {
			if e == nil || e.To == "" {
				continue
			}
			if graph.IsUnresolvedTarget(e.To) {
				priorUnresolved = append(priorUnresolved, &graph.Edge{
					From: e.From, Kind: e.Kind, To: e.To, Meta: e.Meta,
				})
				continue
			}
			if !reusableResolvedEdge(e) {
				continue
			}
			tgt := targets[e.To]
			if tgt == nil || tgt.Name == "" {
				continue
			}
			k := reuseKey{from: e.From, kind: e.Kind, recv: edgeReceiverType(e), name: tgt.Name}
			if cur, seen := reuse[k]; seen {
				if cur != nil && cur.to != e.To {
					reuse[k] = nil // ambiguous -> never reuse
				}
				continue
			}
			reuse[k] = &reuseVal{
				to:         e.To,
				confidence: e.Confidence,
				confLabel:  e.ConfidenceLabel,
				origin:     e.Origin,
				tier:       e.Tier,
			}
		}
	}
	return reuse, priorUnresolved
}

// applyResolvedOutEdges pre-resolves freshly extracted unresolved edges that
// match a captured resolution, BEFORE they are added to the graph (so no edge
// re-keying is needed). Returns how many edges were reused.
//
// newIDs holds the IDs of the nodes about to be re-added by the caller's
// AddBatch. It exists for the same-file case: when a call's target lives in the
// re-parsed file, eviction removed the target node before this runs, so a bare
// point lookup for v.to would miss and the edge would fall through to a full
// re-resolve — which rebinds the target correctly but drops its origin/tier to
// the resolver's heuristic default. Because node IDs are file::Name (line- and
// content-independent), a same-file target re-appears under an identical ID, so
// "present in newIDs" recovers exactly the reuse that a not-yet-added target
// would otherwise lose. A target absent from BOTH the graph and newIDs was
// genuinely deleted and is still skipped.
func applyResolvedOutEdges(g graph.Store, edges []*graph.Edge, idx map[reuseKey]*reuseVal, newIDs map[string]struct{}) int {
	if len(idx) == 0 {
		return 0
	}
	// Determine the small set of captured targets actually referenced by this
	// extraction, then check their existence once. Same-file targets are about
	// to be re-added and deliberately do not need a store lookup.
	targetIDSet := make(map[string]struct{})
	for _, e := range edges {
		if e == nil || !graph.IsUnresolvedTarget(e.To) {
			continue
		}
		name := reuseIdentifier(graph.UnresolvedName(e.To))
		if name == "" {
			continue
		}
		v := idx[reuseKey{from: e.From, kind: e.Kind, recv: edgeReceiverType(e), name: name}]
		if v == nil {
			continue
		}
		if _, reAdded := newIDs[v.to]; !reAdded {
			targetIDSet[v.to] = struct{}{}
		}
	}
	targetIDs := make([]string, 0, len(targetIDSet))
	for id := range targetIDSet {
		targetIDs = append(targetIDs, id)
	}
	var existingTargets map[string]struct{}
	if len(targetIDs) > 0 {
		existingTargets = graph.LookupExistingNodeIDs(g, targetIDs)
	}
	reused := 0
	for _, e := range edges {
		if e == nil || !graph.IsUnresolvedTarget(e.To) {
			continue
		}
		name := reuseIdentifier(graph.UnresolvedName(e.To))
		if name == "" {
			continue // import/pyrel/grpc targets are owned by global passes
		}
		k := reuseKey{from: e.From, kind: e.Kind, recv: edgeReceiverType(e), name: name}
		v := idx[k]
		if v == nil {
			continue // miss, or poisoned ambiguous key
		}
		if _, exists := existingTargets[v.to]; !exists {
			if _, reAdded := newIDs[v.to]; !reAdded {
				continue // captured target genuinely deleted since the snapshot
			}
			// Same-file target: evicted above, re-added by AddBatch below under
			// an identical ID — reuse its prior resolution and provenance tier.
		}
		e.To = v.to
		e.Confidence = v.confidence
		e.ConfidenceLabel = v.confLabel
		e.Origin = v.origin
		e.Tier = v.tier
		reused++
	}
	return reused
}

func reusableResolvedEdge(e *graph.Edge) bool {
	if e == nil || e.To == "" {
		return false
	}
	if graph.IsUnresolvedTarget(e.To) || strings.HasPrefix(e.To, "external::") {
		return false
	}
	return true
}

func edgeReceiverType(e *graph.Edge) string {
	if e == nil || e.Meta == nil {
		return ""
	}
	if rt, ok := e.Meta["receiver_type"].(string); ok {
		return rt
	}
	// Builtin receivers ride their own stamp (kept out of receiver_type
	// for the receiver-gate passes). The reuse key must see the same
	// evidence fresh resolution acts on — blind reuse would re-pin a
	// stale typed bind after an edit changes the local's builtin type.
	if rb, ok := e.Meta["receiver_builtin"].(string); ok {
		return rb
	}
	return ""
}

// reuseIdentifier mirrors resolver.identifierFromTarget for the reuse key:
// it extracts the bare referenced name and returns "" for targets a dedicated
// global pass owns (import/pyrel/grpc), so those are never reused here.
func reuseIdentifier(target string) string {
	switch {
	case strings.HasPrefix(target, "*."):
		return strings.TrimPrefix(target, "*.")
	case strings.HasPrefix(target, "extern::"):
		spec := strings.TrimPrefix(target, "extern::")
		if sep := strings.LastIndex(spec, "::"); sep >= 0 {
			return spec[sep+2:]
		}
		return ""
	case strings.HasPrefix(target, "import::"),
		strings.HasPrefix(target, "pyrel::"),
		strings.HasPrefix(target, "grpc::"):
		return ""
	}
	return target
}
