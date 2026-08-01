package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type attributionIdentityProjectionStore struct {
	graph.Store

	edges          []*graph.Edge
	identityScans  int
	fullScans      int
	scanBatchSize  int
	findBatchSizes []int
	reindexBatches [][]graph.EdgeReindex
}

func (s *attributionIdentityProjectionStore) ScanUnresolvedEdgeIdentitiesBatched(
	_ []graph.EdgeKind,
	batchSize int,
	yield func([]graph.EdgeIdentity) bool,
) {
	s.identityScans++
	s.scanBatchSize = batchSize
	for start := 0; start < len(s.edges); start += batchSize {
		end := start + batchSize
		if end > len(s.edges) {
			end = len(s.edges)
		}
		page := make([]graph.EdgeIdentity, 0, end-start)
		for _, edge := range s.edges[start:end] {
			page = append(page, graph.EdgeIdentityFor(edge))
		}
		if !yield(page) {
			return
		}
	}
}

func (s *attributionIdentityProjectionStore) ScanEdgesByKindsBatched(
	_ []graph.EdgeKind,
	_ int,
	_ func([]*graph.Edge) bool,
) {
	s.fullScans++
}

func (s *attributionIdentityProjectionStore) FindEdgesByIdentities(
	identities []graph.EdgeIdentity,
) map[graph.EdgeIdentity]*graph.Edge {
	s.findBatchSizes = append(s.findBatchSizes, len(identities))
	wanted := make(map[graph.EdgeIdentity]struct{}, len(identities))
	for _, identity := range identities {
		wanted[identity] = struct{}{}
	}
	found := make(map[graph.EdgeIdentity]*graph.Edge, len(identities))
	for _, edge := range s.edges {
		identity := graph.EdgeIdentityFor(edge)
		if _, ok := wanted[identity]; ok {
			found[identity] = edge
		}
	}
	return found
}

func (s *attributionIdentityProjectionStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexBatches = append(s.reindexBatches, append([]graph.EdgeReindex(nil), batch...))
}

func TestReindexAttributionUsesBoundedIdentityDiscoveryAndExactRefetch(t *testing.T) {
	const count = attributionReindexBatchSize*2 + 7
	store := &attributionIdentityProjectionStore{edges: make([]*graph.Edge, 0, count)}
	for i := 0; i < count; i++ {
		store.edges = append(store.edges, &graph.Edge{
			From: fmt.Sprintf("from-%d", i),
			To:   fmt.Sprintf("unresolved::target-%d", i),
			Kind: graph.EdgeReferences,
			Meta: map[string]any{"payload": i},
		})
	}

	resolver := &Resolver{graph: store}
	resolver.reindexAttributionEdgesBatched(
		[]graph.EdgeKind{graph.EdgeReads, graph.EdgeReferences},
		func(edge *graph.Edge) string {
			oldTo := edge.To
			edge.To = "resolved::" + edge.From
			return oldTo
		},
	)

	if store.identityScans != 1 {
		t.Fatalf("identity scans = %d, want 1", store.identityScans)
	}
	if store.fullScans != 0 {
		t.Fatalf("full edge scans = %d, want 0", store.fullScans)
	}
	if store.scanBatchSize != attributionReindexBatchSize {
		t.Fatalf("identity page bound = %d, want %d", store.scanBatchSize, attributionReindexBatchSize)
	}
	if len(store.findBatchSizes) != 3 {
		t.Fatalf("exact refetch batches = %v, want three", store.findBatchSizes)
	}
	for _, size := range store.findBatchSizes {
		if size <= 0 || size > attributionReindexBatchSize {
			t.Fatalf("exact refetch batch size = %d, bound %d", size, attributionReindexBatchSize)
		}
	}
	assertAttributionReindexBatches(t, store.reindexBatches, count)
}
