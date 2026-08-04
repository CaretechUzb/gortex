package resolver

import (
	"iter"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

const overrideDispatchExactBatchSize = 256

type overrideDispatchCandidates struct {
	java  []graph.EdgeIdentity
	php   []graph.EdgeIdentity
	exact bool
}

// collectOverrideDispatchCandidates scans the Java/PHP unresolved-call census
// once. Production SQLite supplies a compact language-gated projection; the
// in-memory graph uses the compatibility fallback. Stores without exact-key
// lookup retain the legacy per-language full scan.
func (r *Resolver) collectOverrideDispatchCandidates() overrideDispatchCandidates {
	if r == nil || r.graph == nil {
		return overrideDispatchCandidates{}
	}
	if _, ok := r.graph.(graph.EdgeIdentityBatchFinder); !ok {
		return overrideDispatchCandidates{}
	}

	var repoPrefixes []string
	if len(r.scope) > 0 {
		repoPrefixes = make([]string, 0, len(r.scope))
		for prefix := range r.scope {
			repoPrefixes = append(repoPrefixes, prefix)
		}
		sort.Strings(repoPrefixes)
	}

	candidates := overrideDispatchCandidates{exact: true}
	graph.ScanOverrideDispatchCalls(
		r.graph,
		repoPrefixes,
		graph.DefaultOverrideDispatchCallPageSize,
		func(page []graph.OverrideDispatchCall) bool {
			for _, row := range page {
				switch row.CallerLanguage {
				case "java":
					candidates.java = append(candidates.java, row.EdgeIdentity)
				case "php":
					candidates.php = append(candidates.php, row.EdgeIdentity)
				}
			}
			return true
		},
	)
	return candidates
}

func (c overrideDispatchCandidates) javaEdges(g graph.Store) iter.Seq[*graph.Edge] {
	return c.edges(g, c.java)
}

func (c overrideDispatchCandidates) phpEdges(g graph.Store) iter.Seq[*graph.Edge] {
	return c.edges(g, c.php)
}

// edges exact-refetches bounded identity batches after the projection cursor
// has closed. Removed or retargeted rows are absent and therefore cannot be
// mutated from a stale census. The legacy path is used only by adapters that do
// not implement exact identity lookup.
func (c overrideDispatchCandidates) edges(
	g graph.Store,
	identities []graph.EdgeIdentity,
) iter.Seq[*graph.Edge] {
	if !c.exact {
		if g == nil {
			return func(func(*graph.Edge) bool) {}
		}
		return g.EdgesByKind(graph.EdgeCalls)
	}
	finder, ok := g.(graph.EdgeIdentityBatchFinder)
	if !ok || len(identities) == 0 {
		return func(func(*graph.Edge) bool) {}
	}
	return func(yield func(*graph.Edge) bool) {
		for start := 0; start < len(identities); start += overrideDispatchExactBatchSize {
			end := min(start+overrideDispatchExactBatchSize, len(identities))
			batch := identities[start:end]
			current := finder.FindEdgesByIdentities(batch)
			for _, identity := range batch {
				edge := current[identity]
				if edge != nil && !yield(edge) {
					return
				}
			}
		}
	}
}
