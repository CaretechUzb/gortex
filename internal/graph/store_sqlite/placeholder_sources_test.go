package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestReconcilePlaceholderSourcesUsesExactSiteAndPreservesPayload(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const (
		oldFrom = "repo/unresolved::*.id"
		newFrom = "repo/model.go::Record.id"
		to      = "repo/use.go::Use"
	)
	store.AddBatch(nil, []*graph.Edge{
		{
			From: oldFrom, To: to, Kind: graph.EdgeArgOf,
			FilePath: "repo/use.go", Line: 17, Confidence: 0.91,
			Meta: map[string]any{"arg_position": 2, "keep": strings.Repeat("x", 4096)},
		},
		{From: oldFrom, To: to, Kind: graph.EdgeValueFlow, FilePath: "repo/use.go", Line: 19},
		{From: oldFrom, To: to, Kind: graph.EdgeReferences, FilePath: "repo/use.go", Line: 17},
	})

	moved := graph.ReconcilePlaceholderSources(store, []graph.PlaceholderRepoint{{
		OldFrom: oldFrom, NewFrom: newFrom, FilePath: "repo/use.go", Line: 17,
	}})
	require.Equal(t, 1, moved)

	resolved := store.GetOutEdges(newFrom)
	require.Len(t, resolved, 1)
	assert.Equal(t, graph.EdgeArgOf, resolved[0].Kind)
	assert.Equal(t, 0.91, resolved[0].Confidence)
	assert.Equal(t, 2, resolved[0].Meta["arg_position"])
	assert.Equal(t, strings.Repeat("x", 4096), resolved[0].Meta["keep"])

	remaining := store.GetOutEdges(oldFrom)
	require.Len(t, remaining, 2)
	assert.ElementsMatch(t, []graph.EdgeKind{graph.EdgeValueFlow, graph.EdgeReferences}, []graph.EdgeKind{
		remaining[0].Kind, remaining[1].Kind,
	})
}

var placeholderAdjacencyBenchmarkSink map[string][]*graph.Edge
var placeholderCandidatesBenchmarkSink graph.EdgeCandidateSet
var placeholderIdentityBenchmarkSink map[graph.EdgeIdentity]*graph.Edge
var placeholderIdentityCensusBenchmarkSink []graph.EdgeIdentity

func BenchmarkPlaceholderSourceCandidateProjection(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "graph.sqlite"))
	require.NoError(b, err)
	b.Cleanup(func() { require.NoError(b, store.Close()) })

	const oldFrom = "repo/unresolved::*.id"
	edges := make([]*graph.Edge, 0, 4097)
	payload := strings.Repeat("metadata", 256)
	for i := 0; i < 4096; i++ {
		edges = append(edges, &graph.Edge{
			From: oldFrom, To: fmt.Sprintf("repo/target-%05d", i),
			Kind: graph.EdgeReferences, FilePath: "repo/use.go", Line: i + 100,
			Meta: map[string]any{"payload": payload},
		})
	}
	edges = append(edges, &graph.Edge{
		From: oldFrom, To: "repo/use.go::Use", Kind: graph.EdgeArgOf,
		FilePath: "repo/use.go", Line: 17, Meta: map[string]any{"payload": payload},
	})
	store.AddBatch(nil, edges)

	b.Run("full-adjacency", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			placeholderAdjacencyBenchmarkSink = store.GetOutEdgesByNodeIDs([]string{oldFrom})
		}
	})
	b.Run("exact-site", func(b *testing.B) {
		b.ReportAllocs()
		sites := []graph.EdgeSite{{From: oldFrom, Line: 17, Kind: graph.EdgeArgOf}}
		for i := 0; i < b.N; i++ {
			placeholderCandidatesBenchmarkSink = store.GetEdgeCandidates(nil, sites)
		}
	})

	const denseFrom = "repo/unresolved::dense"
	denseEdges := make([]*graph.Edge, 0, 1024)
	denseSites := make([]graph.EdgeSite, 0, 1024)
	for i := 0; i < 1024; i++ {
		denseEdges = append(denseEdges, &graph.Edge{
			From: denseFrom, To: fmt.Sprintf("repo/dense-%05d", i),
			Kind: graph.EdgeArgOf, FilePath: "repo/dense.go", Line: i + 1,
			Meta: map[string]any{"payload": payload},
		})
		denseSites = append(denseSites, graph.EdgeSite{From: denseFrom, Line: i + 1, Kind: graph.EdgeArgOf})
	}
	store.AddBatch(nil, denseEdges)
	b.Run("shared-placeholder-repeated-adjacency", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for start := 0; start < len(denseSites); start += 32 {
				placeholderAdjacencyBenchmarkSink = store.GetOutEdgesByNodeIDs([]string{denseFrom})
			}
		}
	})
	b.Run("shared-placeholder-indexed-refetch", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			identities := make([]graph.EdgeIdentity, 0, len(denseSites))
			for edge := range graph.EdgesLightSeq(store, graph.EdgeArgOf, graph.EdgeValueFlow) {
				if edge != nil && edge.From == denseFrom {
					identities = append(identities, graph.EdgeIdentityFor(edge))
				}
			}
			placeholderIdentityCensusBenchmarkSink = identities
			for start := 0; start < len(identities); start += 32 {
				end := start + 32
				if end > len(identities) {
					end = len(identities)
				}
				placeholderIdentityBenchmarkSink = store.FindEdgesByIdentities(identities[start:end])
			}
		}
	})
}
