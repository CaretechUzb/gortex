package resolver

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type placeholderIdentityTrackingStore struct {
	*graph.Graph
	identityBatchSizes []int
	candidateCalls     int
	adjacencyCalls     int
}

func (s *placeholderIdentityTrackingStore) FindEdgesByIdentities(
	identities []graph.EdgeIdentity,
) map[graph.EdgeIdentity]*graph.Edge {
	s.identityBatchSizes = append(s.identityBatchSizes, len(identities))
	return s.Graph.FindEdgesByIdentities(identities)
}

func (s *placeholderIdentityTrackingStore) GetEdgeCandidates(
	endpoints []graph.EdgeEndpoint,
	sites []graph.EdgeSite,
) graph.EdgeCandidateSet {
	s.candidateCalls++
	return s.Graph.GetEdgeCandidates(endpoints, sites)
}

func (s *placeholderIdentityTrackingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.adjacencyCalls++
	return s.Graph.GetOutEdgesByNodeIDs(ids)
}

func TestPlaceholderSourceIndexBatchesSharedPlaceholderIdentities(t *testing.T) {
	const (
		count      = 600
		oldFrom    = "unresolved::*.id"
		filePath   = "repo/use.go"
		metaMarker = "payload-that-must-survive"
	)
	g := graph.New()
	store := &placeholderIdentityTrackingStore{Graph: g}
	edges := make([]*graph.Edge, 0, count)
	reindexes := make([]graph.EdgeReindex, 0, count)
	for i := 0; i < count; i++ {
		kind := graph.EdgeArgOf
		if i%2 != 0 {
			kind = graph.EdgeValueFlow
		}
		edges = append(edges, &graph.Edge{
			From: oldFrom, To: fmt.Sprintf("repo/callee-%03d", i), Kind: kind,
			FilePath: filePath, Line: i + 1,
			Meta: map[string]any{"keep": metaMarker, "payload": strings.Repeat("x", 256)},
		})
		reindexes = append(reindexes, graph.EdgeReindex{
			Edge: &graph.Edge{
				From: fmt.Sprintf("caller-%03d", i), To: fmt.Sprintf("repo/field-%03d", i),
				Kind: graph.EdgeReferences, FilePath: filePath, Line: i + 1,
			},
			OldTo: oldFrom,
		})
	}
	g.AddBatch(nil, edges)

	idx := &placeholderSourceIndex{}
	moved := reconcilePlaceholderSources(store, idx, reindexes)
	require.Equal(t, count, moved)
	assert.Equal(t, []int{256, 256, 88}, store.identityBatchSizes)
	assert.Zero(t, store.candidateCalls, "full-pass identity census must avoid per-site SQL probes")
	assert.Zero(t, store.adjacencyCalls, "shared placeholders must never decode full adjacency per chunk")
	assert.Empty(t, g.GetOutEdges(oldFrom))
	for _, sample := range []int{0, 255, 256, count - 1} {
		out := g.GetOutEdges(fmt.Sprintf("repo/field-%03d", sample))
		require.Lenf(t, out, 1, "sample %d", sample)
		assert.Equal(t, metaMarker, out[0].Meta["keep"])
	}
}

func TestPlaceholderSourceIndexPreservesFirstRepointAtSharedSite(t *testing.T) {
	const (
		oldFrom  = "unresolved::shared"
		filePath = "repo/use.go"
	)
	g := graph.New()
	store := &placeholderIdentityTrackingStore{Graph: g}
	g.AddEdge(&graph.Edge{
		From: oldFrom, To: "repo/callee", Kind: graph.EdgeArgOf,
		FilePath: filePath, Line: 7,
	})
	reindexes := []graph.EdgeReindex{
		{Edge: &graph.Edge{From: "caller-a", To: "repo/first", FilePath: filePath, Line: 7}, OldTo: oldFrom},
		{Edge: &graph.Edge{From: "caller-b", To: "repo/second", FilePath: filePath, Line: 7}, OldTo: oldFrom},
	}

	moved := reconcilePlaceholderSources(store, &placeholderSourceIndex{}, reindexes)
	require.Equal(t, 1, moved)
	assert.Len(t, g.GetOutEdges("repo/first"), 1)
	assert.Empty(t, g.GetOutEdges("repo/second"))
}
