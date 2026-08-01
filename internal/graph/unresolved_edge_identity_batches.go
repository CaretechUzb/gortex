package graph

// UnresolvedEdgeIdentityBatchScanner is an optional compact projection for
// mutation passes whose rewrite predicate starts with an unresolved target.
// Implementations return only logical edge identities, close every backend
// cursor before yield, and freeze a high-water so callback rewrites cannot
// re-enter the same scan.
type UnresolvedEdgeIdentityBatchScanner interface {
	ScanUnresolvedEdgeIdentitiesBatched(
		kinds []EdgeKind,
		batchSize int,
		yield func([]EdgeIdentity) bool,
	)
}

// ScanUnresolvedEdgeIdentitiesBatched preserves the in-memory Graph scanner's
// mutation-stable order while projecting only identities. Full edges already
// live in Graph, so filtering them does not duplicate payload storage.
func (g *Graph) ScanUnresolvedEdgeIdentitiesBatched(
	kinds []EdgeKind,
	batchSize int,
	yield func([]EdgeIdentity) bool,
) {
	if g == nil || yield == nil {
		return
	}
	kinds = normalizedEdgeBatchKinds(kinds)
	if len(kinds) == 0 {
		return
	}
	if batchSize <= 0 {
		batchSize = 1
	}

	batch := make([]EdgeIdentity, 0, batchSize)
	keepGoing := true
	flush := func() {
		if len(batch) == 0 || !keepGoing {
			return
		}
		keepGoing = yield(batch)
		batch = make([]EdgeIdentity, 0, batchSize)
	}
	g.ScanEdgesByKindsBatched(kinds, batchSize, func(edges []*Edge) bool {
		for _, edge := range edges {
			if edge == nil || !IsUnresolvedTarget(edge.To) {
				continue
			}
			batch = append(batch, EdgeIdentityFor(edge))
			if len(batch) == batchSize {
				flush()
				if !keepGoing {
					return false
				}
			}
		}
		return true
	})
	flush()
}

var _ UnresolvedEdgeIdentityBatchScanner = (*Graph)(nil)
