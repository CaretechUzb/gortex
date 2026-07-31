package resolver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type attributionBatchRecordingStore struct {
	graph.Store

	edges          []*graph.Edge
	scanCalls      int
	scanKinds      []graph.EdgeKind
	scanBatchSize  int
	reindexBatches [][]graph.EdgeReindex
}

func (s *attributionBatchRecordingStore) ScanEdgesByKindsBatched(
	kinds []graph.EdgeKind,
	batchSize int,
	yield func([]*graph.Edge) bool,
) {
	s.scanCalls++
	s.scanKinds = append([]graph.EdgeKind(nil), kinds...)
	s.scanBatchSize = batchSize
	for start := 0; start < len(s.edges); start += batchSize {
		end := start + batchSize
		if end > len(s.edges) {
			end = len(s.edges)
		}
		if !yield(s.edges[start:end]) {
			return
		}
	}
}

func (s *attributionBatchRecordingStore) ReindexEdges(batch []graph.EdgeReindex) {
	copied := append([]graph.EdgeReindex(nil), batch...)
	s.reindexBatches = append(s.reindexBatches, copied)
}

func TestReindexAttributionEdgesBatchedBoundsFullEdges(t *testing.T) {
	const count = attributionReindexBatchSize*2 + 7
	store := &attributionBatchRecordingStore{edges: make([]*graph.Edge, 0, count)}
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

	require.Equal(t, 1, store.scanCalls)
	assert.Equal(t, attributionReindexBatchSize, store.scanBatchSize)
	assert.Equal(t, []graph.EdgeKind{graph.EdgeReads, graph.EdgeReferences}, store.scanKinds)
	assertAttributionReindexBatches(t, store.reindexBatches, count)
}

func TestPersistAttributionReindexesBoundsDirectWrites(t *testing.T) {
	const count = attributionReindexBatchSize*2 + 7
	store := &attributionBatchRecordingStore{}
	resolver := &Resolver{graph: store}

	resolver.persistAttributionReindexes(makeAttributionReindexes(count))

	assertAttributionReindexBatches(t, store.reindexBatches, count)
}

func TestPersistAttributionReindexesBoundsIncrementalQueue(t *testing.T) {
	const count = attributionReindexBatchSize*2 + 7
	store := &attributionBatchRecordingStore{}
	resolver := &Resolver{
		graph:                  store,
		incrementalNodesByFile: map[string][]*graph.Node{},
	}

	resolver.persistAttributionReindexes(makeAttributionReindexes(count))

	require.Len(t, store.reindexBatches, 2, "full chunks should flush immediately")
	assert.Len(t, resolver.incrementalAttributionReindex, 7, "only the bounded tail should remain queued")
	resolver.flushIncrementalAttributionReindexes()
	assert.Nil(t, resolver.incrementalAttributionReindex)
	assertAttributionReindexBatches(t, store.reindexBatches, count)
}

func makeAttributionReindexes(count int) []graph.EdgeReindex {
	batch := make([]graph.EdgeReindex, 0, count)
	for i := 0; i < count; i++ {
		batch = append(batch, graph.EdgeReindex{
			Edge:  &graph.Edge{From: fmt.Sprintf("from-%d", i)},
			OldTo: fmt.Sprintf("old-%d", i),
		})
	}
	return batch
}

func assertAttributionReindexBatches(t *testing.T, batches [][]graph.EdgeReindex, count int) {
	t.Helper()
	seen := 0
	for _, batch := range batches {
		require.NotEmpty(t, batch)
		assert.LessOrEqual(t, len(batch), attributionReindexBatchSize)
		for _, rewrite := range batch {
			assert.Equal(t, fmt.Sprintf("from-%d", seen), rewrite.Edge.From)
			if rewrite.OldTo != fmt.Sprintf("old-%d", seen) &&
				rewrite.OldTo != fmt.Sprintf("unresolved::target-%d", seen) {
				t.Fatalf("rewrite %d has unexpected OldTo %q", seen, rewrite.OldTo)
			}
			seen++
		}
	}
	assert.Equal(t, count, seen)
}
