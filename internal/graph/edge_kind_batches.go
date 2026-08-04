package graph

// EdgesByKindsBatchScanner is an optional capability for mutation passes that
// need full edge payloads in bounded pages. Implementations must close the
// read cursor before invoking yield. Backends whose rewrites insert new rows
// should also freeze a scan high-water so those rows cannot re-enter the same
// pass.
type EdgesByKindsBatchScanner interface {
	ScanEdgesByKindsBatched(kinds []EdgeKind, batchSize int, yield func([]*Edge) bool)
}

var _ EdgesByKindsBatchScanner = (*Graph)(nil)

// ScanEdgesByKindsBatched is the mutation-stable in-memory implementation.
// It snapshots source bucket names before visiting a shard and remembers each
// yielded pointer. A callback may therefore reindex an edge into a bucket or
// shard that has not yet been visited without causing a second yield. Only one
// source bucket and one callback batch of edge pointers are materialized at a
// time; the seen set is pointer-only and does not duplicate edge payloads the
// graph already owns.
func (g *Graph) ScanEdgesByKindsBatched(
	kinds []EdgeKind,
	batchSize int,
	yield func([]*Edge) bool,
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
	kindSet := make(map[EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		kindSet[kind] = struct{}{}
	}

	seen := make(map[*Edge]struct{})
	batch := make([]*Edge, 0, batchSize)
	flush := func() bool {
		if len(batch) == 0 {
			return true
		}
		keepGoing := yield(batch)
		batch = make([]*Edge, 0, batchSize)
		return keepGoing
	}

	for _, shard := range g.shards {
		shard.mu.RLock()
		sourceIDs := make([]string, 0, len(shard.outEdges))
		for sourceID := range shard.outEdges {
			sourceIDs = append(sourceIDs, sourceID)
		}
		shard.mu.RUnlock()

		for _, sourceID := range sourceIDs {
			shard.mu.RLock()
			edges := append([]*Edge(nil), shard.outEdges[sourceID]...)
			shard.mu.RUnlock()
			for _, edge := range edges {
				if edge == nil {
					continue
				}
				if _, duplicate := seen[edge]; duplicate {
					continue
				}
				if _, selected := kindSet[edge.Kind]; !selected {
					continue
				}
				seen[edge] = struct{}{}
				batch = append(batch, edge)
				if len(batch) == batchSize && !flush() {
					return
				}
			}
		}
	}
	flush()
}

// ScanEdgesByKindsBatched selects the strong backend capability when present
// and otherwise batches the existing kind-filtered iterator. Empty and blank
// kind sets do no work; duplicate kinds never duplicate rows. A non-positive
// batch size is normalized to one.
func ScanEdgesByKindsBatched(
	s Store,
	kinds []EdgeKind,
	batchSize int,
	yield func([]*Edge) bool,
) {
	if s == nil || yield == nil {
		return
	}
	kinds = normalizedEdgeBatchKinds(kinds)
	if len(kinds) == 0 {
		return
	}
	if batchSize <= 0 {
		batchSize = 1
	}
	if scanner, ok := s.(EdgesByKindsBatchScanner); ok {
		scanner.ScanEdgesByKindsBatched(kinds, batchSize, yield)
		return
	}

	batch := make([]*Edge, 0, batchSize)
	keepGoing := true
	flush := func() {
		if len(batch) == 0 || !keepGoing {
			return
		}
		keepGoing = yield(batch)
		batch = make([]*Edge, 0, batchSize)
	}
	appendEdge := func(edge *Edge) bool {
		if edge == nil {
			return true
		}
		batch = append(batch, edge)
		if len(batch) == batchSize {
			flush()
		}
		return keepGoing
	}

	if scanner, ok := s.(EdgesByKindsScanner); ok {
		for edge := range scanner.EdgesByKinds(kinds) {
			if !appendEdge(edge) {
				return
			}
		}
	} else {
		for _, kind := range kinds {
			for edge := range s.EdgesByKind(kind) {
				if !appendEdge(edge) {
					return
				}
			}
		}
	}
	flush()
}

func normalizedEdgeBatchKinds(kinds []EdgeKind) []EdgeKind {
	seen := make(map[EdgeKind]struct{}, len(kinds))
	uniq := make([]EdgeKind, 0, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}
		uniq = append(uniq, kind)
	}
	return uniq
}
