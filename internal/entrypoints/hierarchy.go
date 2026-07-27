package entrypoints

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// hierarchyEntryKind registers an entry-point kind a base class confers
// on every derived type — a hole per-file detection cannot see when the
// base's own base list lives in another file, in any language.
type hierarchyEntryKind struct {
	kind     string // MetaEntryKind value that seeds a walk and is stamped downward
	language string // extractor language whose KindType nodes participate
}

// aspnet:controller first: `ClaimController : BaseController` routes
// like a controller with nothing controller-shaped in its own file —
// 44 of 119 controllers on the reference codebase. Other frameworks'
// analogues slot in without touching the pass.
var hierarchyEntryKinds = []hierarchyEntryKind{
	{kind: "aspnet:controller", language: "csharp"},
}

// hierarchyLookupBatchSize bounds GetNodesByIDs / GetInEdgesByNodeIDs
// batches, mirroring the indexer's test-metadata lookup cap.
const hierarchyLookupBatchSize = 512

// PropagateEntryPointsDownHierarchy stamps every registered kind down
// resolved extends chains, whole-graph (cold index / end-of-batch; the
// watcher path uses the Scoped variant). Inverted walk: seeds are the
// types Detect already stamped, followed via INCOMING extends edges —
// O(seed subtree), and nothing at all when the store lacks the kind's
// language. Returns the number of newly stamped types.
func PropagateEntryPointsDownHierarchy(g graph.Store) int {
	if g == nil {
		return 0
	}
	total := 0
	for _, hk := range hierarchyEntryKinds {
		if !storeHasLanguage(g, hk.language) {
			continue
		}
		seeds := make(map[string]bool)
		for n := range nodesByKindLang(g, graph.KindType, hk.language) {
			if entryKindOf(n) == hk.kind {
				seeds[n.ID] = true
			}
		}
		total += propagateFromSeeds(g, hk, seeds)
	}
	return total
}

// PropagateEntryPointsDownHierarchyScoped is the incremental form,
// driven by the changed-type frontier: a stamped frontier type seeds a
// downward walk, an unstamped one first climbs to a stamped ancestor
// (a new derived type hung off an unchanged hierarchy). Frontier IDs
// are exact — no repo-prefix readers — and both walks follow resolved
// edges wherever they lead: changed repos plus what they depend on,
// never a whole-graph scan.
func PropagateEntryPointsDownHierarchyScoped(g graph.Store, typeIDs []string) int {
	if g == nil || len(typeIDs) == 0 {
		return 0
	}
	frontier := hydrateNodes(g, typeIDs)
	total := 0
	for _, hk := range hierarchyEntryKinds {
		seeds := make(map[string]bool)
		var unstamped []string
		for _, n := range frontier {
			if n == nil || n.Kind != graph.KindType || n.Language != hk.language {
				continue
			}
			switch entryKindOf(n) {
			case hk.kind:
				seeds[n.ID] = true
			case "":
				unstamped = append(unstamped, n.ID)
			}
		}
		if len(seeds) == 0 && len(unstamped) == 0 {
			continue // implicit language gate: no candidate in the frontier
		}
		for id := range stampedAncestors(g, hk, unstamped) {
			seeds[id] = true
		}
		total += propagateFromSeeds(g, hk, seeds)
	}
	return total
}

// stampedAncestors climbs resolved extends chains and returns ancestors
// already stamped with hk.kind. The visited set caps cycles; a type
// stamped with a different kind ends its path.
func stampedAncestors(g graph.Store, hk hierarchyEntryKind, ids []string) map[string]bool {
	found := make(map[string]bool)
	visited := make(map[string]bool, len(ids))
	frontier := append([]string(nil), ids...)
	for len(frontier) > 0 {
		var baseIDs []string
		for _, chunk := range chunkIDs(frontier) {
			for _, edges := range g.GetOutEdgesByNodeIDs(chunk) {
				for _, e := range edges {
					if e == nil || e.Kind != graph.EdgeExtends ||
						graph.IsUnresolvedTarget(e.To) || visited[e.To] {
						continue
					}
					visited[e.To] = true
					baseIDs = append(baseIDs, e.To)
				}
			}
		}
		frontier = frontier[:0]
		for _, n := range hydrateNodes(g, baseIDs) {
			if n == nil || n.Kind != graph.KindType || n.Language != hk.language {
				continue
			}
			switch entryKindOf(n) {
			case hk.kind:
				found[n.ID] = true // seed; its subtree walk covers the rest
			case "":
				frontier = append(frontier, n.ID) // plain base — keep climbing
			}
		}
	}
	return found
}

// propagateFromSeeds BFSes down incoming resolved extends edges,
// stamping same-language KindType descendants with no entry-point kind
// yet (a different kind — a derived xunit fixture — ends the branch).
// Fetched nodes are decoded copies on the SQLite backend, so the stamps
// only land via the batch upsert. Idempotent.
func propagateFromSeeds(g graph.Store, hk hierarchyEntryKind, seeds map[string]bool) int {
	if len(seeds) == 0 {
		return 0
	}
	visited := make(map[string]bool, len(seeds))
	frontier := make([]string, 0, len(seeds))
	for id := range seeds {
		visited[id] = true
		frontier = append(frontier, id)
	}
	var changed []*graph.Node
	for len(frontier) > 0 {
		var derivedIDs []string
		for _, chunk := range chunkIDs(frontier) {
			for _, edges := range g.GetInEdgesByNodeIDs(chunk) {
				for _, e := range edges {
					if e == nil || e.Kind != graph.EdgeExtends || visited[e.From] {
						continue
					}
					visited[e.From] = true
					derivedIDs = append(derivedIDs, e.From)
				}
			}
		}
		frontier = frontier[:0]
		for _, n := range hydrateNodes(g, derivedIDs) {
			if n == nil || n.Kind != graph.KindType || n.Language != hk.language {
				continue
			}
			switch entryKindOf(n) {
			case hk.kind:
				frontier = append(frontier, n.ID) // already stamped; walk on
			case "":
				stamp(n, hk.kind)
				changed = append(changed, n)
				frontier = append(frontier, n.ID)
			}
		}
	}
	if len(changed) > 0 {
		g.ResolveMutex().Lock()
		g.AddBatch(changed, nil)
		g.ResolveMutex().Unlock()
	}
	return len(changed)
}

func entryKindOf(n *graph.Node) string {
	if n == nil || n.Meta == nil {
		return ""
	}
	kind, _ := n.Meta[MetaEntryKind].(string)
	return kind
}

func hydrateNodes(g graph.Store, ids []string) []*graph.Node {
	var out []*graph.Node
	for _, chunk := range chunkIDs(ids) {
		for _, n := range g.GetNodesByIDs(chunk) {
			out = append(out, n)
		}
	}
	return out
}

func chunkIDs(ids []string) [][]string {
	var out [][]string
	for start := 0; start < len(ids); start += hierarchyLookupBatchSize {
		end := min(start+hierarchyLookupBatchSize, len(ids))
		out = append(out, ids[start:end])
	}
	return out
}

// storeHasLanguage / nodesByKindLang mirror the resolver's optional-
// interface probes (resolver/language_gate.go): pushed server-side on
// stores that implement them, conservative / filtered in Go otherwise.
func storeHasLanguage(g graph.Store, lang string) bool {
	if hl, ok := g.(interface{ HasLanguage(string) bool }); ok {
		return hl.HasLanguage(lang)
	}
	return true
}

func nodesByKindLang(g graph.Store, kind graph.NodeKind, lang string) iter.Seq[*graph.Node] {
	if nl, ok := g.(interface {
		NodesByKindLang(graph.NodeKind, string) iter.Seq[*graph.Node]
	}); ok {
		return nl.NodesByKindLang(kind, lang)
	}
	return func(yield func(*graph.Node) bool) {
		for n := range g.NodesByKind(kind) {
			if n != nil && n.Language == lang {
				if !yield(n) {
					return
				}
			}
		}
	}
}
