package indexer

import (
	"github.com/zzet/gortex/internal/graph"
)

// propagateAspNetControllerHierarchy stamps aspnet:controller down the
// resolved C# inheritance chain. Extraction-time detection
// (entrypoints.detectDotNetFramework) sees one file at a time, so it
// stamps a type that carries [ApiController]/[Controller] or names
// Controller / ControllerBase in its OWN base list — but a controller
// that extends a project-local base (`ClaimController : BaseController`)
// shows nothing controller-shaped in its file and is missed; on a
// measured production codebase that was 44 of 119 controllers, each a
// dead-code false positive. This pass runs after the resolver has bound
// base-list targets, walks extends edges downward from every stamped
// controller, and stamps the descendants, so the per-file detector and
// the graph converge on the runtime's actual routing set.
//
// Gates mirror the extraction-time detector: csharp KindType nodes
// only, and only the aspnet:controller entry-point kind seeds a walk —
// a derived xunit test class must not become a controller. Unresolved
// extends targets contribute nothing. Idempotent: already-stamped
// descendants neither re-persist nor re-count.
//
// Returns the number of newly stamped types.
func propagateAspNetControllerHierarchy(g graph.Store) int {
	if g == nil {
		return 0
	}
	// One extends scan builds the downward adjacency (base -> derived).
	derivedByBase := make(map[string][]string)
	ids := make(map[string]struct{})
	for e := range g.EdgesByKind(graph.EdgeExtends) {
		if e == nil || graph.IsUnresolvedTarget(e.To) {
			continue
		}
		derivedByBase[e.To] = append(derivedByBase[e.To], e.From)
		ids[e.From] = struct{}{}
		ids[e.To] = struct{}{}
	}
	if len(derivedByBase) == 0 {
		return 0
	}

	// Hydrate every node on a hierarchy edge in bounded batches.
	batch := make([]string, 0, testMetadataLookupBatchSize)
	nodes := make(map[string]*graph.Node, len(ids))
	flush := func() {
		if len(batch) == 0 {
			return
		}
		for _, n := range g.GetNodesByIDs(batch) {
			if n != nil {
				nodes[n.ID] = n
			}
		}
		batch = batch[:0]
	}
	for id := range ids {
		batch = append(batch, id)
		if len(batch) == testMetadataLookupBatchSize {
			flush()
		}
	}
	flush()

	isCSharpType := func(n *graph.Node) bool {
		return n != nil && n.Kind == graph.KindType && n.Language == "csharp"
	}
	isController := func(n *graph.Node) bool {
		if n == nil || n.Meta == nil {
			return false
		}
		kind, _ := n.Meta["entry_point_kind"].(string)
		return kind == "aspnet:controller"
	}

	// BFS downward from every extraction-stamped controller.
	var queue []string
	for id, n := range nodes {
		if isCSharpType(n) && isController(n) {
			queue = append(queue, id)
		}
	}
	var changed []*graph.Node
	stamped := 0
	for len(queue) > 0 {
		baseID := queue[0]
		queue = queue[1:]
		for _, derivedID := range derivedByBase[baseID] {
			n := nodes[derivedID]
			if !isCSharpType(n) || isController(n) {
				continue
			}
			if n.Meta == nil {
				n.Meta = map[string]any{}
			}
			n.Meta["entry_point"] = true
			n.Meta["entry_point_kind"] = "aspnet:controller"
			changed = append(changed, n)
			stamped++
			queue = append(queue, derivedID)
		}
	}
	if len(changed) > 0 {
		// Persist through the store like the test-symbol stamping pass:
		// fetched nodes are decoded copies on the SQLite backend, so the
		// Meta writes above only land via an explicit batch upsert.
		g.ResolveMutex().Lock()
		g.AddBatch(changed, nil)
		g.ResolveMutex().Unlock()
	}
	return stamped
}
