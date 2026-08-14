package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
)

type exploreSourceLiteralUnsupportedSiteStore struct {
	graph.Store
}

func (s *exploreSourceLiteralUnsupportedSiteStore) FindFileNodesBounded(
	ctx context.Context,
	path string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	return s.Store.(graph.BoundedFileNodeReader).FindFileNodesBounded(ctx, path, scope, limit)
}

func (*exploreSourceLiteralUnsupportedSiteStore) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	panic("legacy outgoing adjacency must not be used")
}

func sourceLiteralCallFixture(
	path string,
	owner *graph.Node,
	line int,
	count int,
) ([]*graph.Node, []*graph.Edge, *graph.Node) {
	nodes := make([]*graph.Node, 0, count+1)
	nodes = append(nodes, owner)
	edges := make([]*graph.Edge, 0, count)
	var wanted *graph.Node
	for index := 0; index < count; index++ {
		name := fmt.Sprintf("Other%d", index)
		if index == 0 {
			name = "Resolve"
		}
		callee := sourceLiteralNode(
			fmt.Sprintf("%s::callee-%d", path, index), name, path,
			graph.KindMethod, 100+index*2, 101+index*2,
		)
		if index == 0 {
			wanted = callee
		}
		nodes = append(nodes, callee)
		edges = append(edges, &graph.Edge{
			From: owner.ID, To: callee.ID, Kind: graph.EdgeCalls,
			FilePath: path, Line: line,
		})
	}
	return nodes, edges, wanted
}

func TestMapExploreSourceLiteralMatchesCallsiteExactEightAndNinthSentinel(t *testing.T) {
	for _, test := range []struct {
		name      string
		callEdges int
		promoted  bool
	}{
		{name: "exact eight is complete", callEdges: exploreSourceLiteralCallEdgesPerSite, promoted: true},
		{name: "ninth fails closed", callEdges: exploreSourceLiteralCallEdgesPerSite + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			const path = "demo/src/calls.cs"
			owner := sourceLiteralNode(path+"::owner", "owner", path, graph.KindMethod, 1, 20)
			nodes, edges, wanted := sourceLiteralCallFixture(path, owner, 3, test.callEdges)
			for index := 0; index < 64; index++ {
				edges = append(edges, &graph.Edge{
					From: owner.ID, To: fmt.Sprintf("irrelevant-%d", index),
					Kind: graph.EdgeReferences, FilePath: path, Line: 3,
				})
			}
			server, counting := newExploreSourceLiteralGraphServer(t, nodes, edges)
			recall := server.mapExploreSourceLiteralMatches("needle", []trigram.Match{{
				Path: path, Line: 3, Text: `Resolve("needle");`,
			}}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})

			wantID := owner.ID
			if test.promoted {
				wantID = wanted.ID
			}
			requireSourceLiteralHitIdentity(t, recall.hits, exploreSourceLiteralHit{
				nodeID: wantID, rank: 0, callee: test.promoted,
			})
			require.Equal(t, 1, counting.outSiteBatchCalls)
			require.Zero(t, counting.outEdgeBatchCalls)
			if test.promoted {
				require.Len(t, counting.lastNodeIDs, exploreSourceLiteralCallEdgesPerSite)
			} else {
				require.Zero(t, counting.nodeLookupBatches)
			}
		})
	}
}

func TestMapExploreSourceLiteralMatchesDropsWholeCalleeLaneOnOneTruncatedSite(t *testing.T) {
	const path = "demo/src/atomic.cs"
	firstOwner := sourceLiteralNode(path+"::first", "first", path, graph.KindMethod, 1, 10)
	secondOwner := sourceLiteralNode(path+"::second", "second", path, graph.KindMethod, 20, 30)
	firstNodes, firstEdges, _ := sourceLiteralCallFixture(path, firstOwner, 3, 1)
	secondNodes, secondEdges, _ := sourceLiteralCallFixture(path, secondOwner, 23, exploreSourceLiteralCallEdgesPerSite+1)
	nodes := append(firstNodes, secondNodes...)
	edges := append(firstEdges, secondEdges...)
	server, counting := newExploreSourceLiteralGraphServer(t, nodes, edges)

	recall := server.mapExploreSourceLiteralMatches("needle", []trigram.Match{
		{Path: path, Line: 3, Text: `Resolve("needle");`},
		{Path: path, Line: 23, Text: `Resolve("needle");`},
	}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})

	requireSourceLiteralHitIdentity(t, recall.hits,
		exploreSourceLiteralHit{nodeID: firstOwner.ID, rank: 0},
		exploreSourceLiteralHit{nodeID: secondOwner.ID, rank: 1},
	)
	require.Zero(t, counting.nodeLookupBatches, "a truncated peer must discard every partial callee proof")
}

func TestMapExploreSourceLiteralMatchesDropsWholeCalleeLaneOnMissingHydration(t *testing.T) {
	const path = "demo/src/hydration.cs"
	firstOwner := sourceLiteralNode(path+"::first", "first", path, graph.KindMethod, 1, 10)
	secondOwner := sourceLiteralNode(path+"::second", "second", path, graph.KindMethod, 20, 30)
	firstNodes, firstEdges, _ := sourceLiteralCallFixture(path, firstOwner, 3, 1)
	secondNodes, secondEdges, _ := sourceLiteralCallFixture(path, secondOwner, 23, 1)
	server, counting := newExploreSourceLiteralGraphServer(t, append(firstNodes, secondNodes...), append(firstEdges, secondEdges...))
	counting.nodeLookup = func(ctx context.Context, ids []string) (map[string]*graph.Node, error) {
		nodes, err := counting.Store.(exploreContextNodesReader).GetNodesByIDsContext(ctx, ids)
		delete(nodes, ids[len(ids)-1])
		return nodes, err
	}

	recall := server.mapExploreSourceLiteralMatches("needle", []trigram.Match{
		{Path: path, Line: 3, Text: `Resolve("needle");`},
		{Path: path, Line: 23, Text: `Resolve("needle");`},
	}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})

	requireSourceLiteralHitIdentity(t, recall.hits,
		exploreSourceLiteralHit{nodeID: firstOwner.ID, rank: 0},
		exploreSourceLiteralHit{nodeID: secondOwner.ID, rank: 1},
	)
	require.Equal(t, 1, counting.nodeLookupBatches)
}

func TestMapExploreSourceLiteralMatchesCapsCalleeHydrationAt192(t *testing.T) {
	const path = "demo/src/many-sites.cs"
	owner := sourceLiteralNode(path+"::owner", "owner", path, graph.KindMethod, 1, 40)
	nodes := []*graph.Node{owner}
	edges := make([]*graph.Edge, 0, exploreSourceLiteralCalleeHydrationLimit)
	matches := make([]trigram.Match, 0, exploreSourceLiteralRecallMaxHits)
	for site := 0; site < exploreSourceLiteralRecallMaxHits; site++ {
		line := site + 2
		matches = append(matches, trigram.Match{Path: path, Line: line, Text: `Resolve("needle");`})
		for index := 0; index < exploreSourceLiteralCallEdgesPerSite; index++ {
			callee := sourceLiteralNode(
				fmt.Sprintf("%s::callee-%d-%d", path, site, index), "Resolve", path,
				graph.KindMethod, 100+site*20+index, 100+site*20+index,
			)
			nodes = append(nodes, callee)
			edges = append(edges, &graph.Edge{
				From: owner.ID, To: callee.ID, Kind: graph.EdgeCalls,
				FilePath: path, Line: line,
			})
		}
	}
	server, counting := newExploreSourceLiteralGraphServer(t, nodes, edges)
	recall := server.mapExploreSourceLiteralMatches(
		"needle", matches, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
	)

	requireSourceLiteralHitIdentity(t, recall.hits, exploreSourceLiteralHit{nodeID: owner.ID, rank: 0})
	require.Equal(t, 1, counting.outSiteBatchCalls)
	require.Len(t, counting.lastSites, exploreSourceLiteralRecallMaxHits)
	require.Equal(t, 1, counting.nodeLookupBatches)
	require.Len(t, counting.lastNodeIDs, exploreSourceLiteralCalleeHydrationLimit)
}

func TestMapExploreSourceLiteralMatchesFailsClosedWithoutSiteCapability(t *testing.T) {
	const path = "demo/src/unsupported.cs"
	owner := sourceLiteralNode(path+"::owner", "owner", path, graph.KindMethod, 1, 10)
	callee := sourceLiteralNode(path+"::Resolve", "Resolve", path, graph.KindMethod, 20, 30)
	server := newExploreSourceLiteralServer(t, []*graph.Node{owner, callee})
	server.graph = &exploreSourceLiteralUnsupportedSiteStore{Store: server.graph}

	recall := server.mapExploreSourceLiteralMatches("needle", []trigram.Match{{
		Path: path, Line: 3, Text: `Resolve("needle");`,
	}}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})
	requireSourceLiteralHitIdentity(t, recall.hits, exploreSourceLiteralHit{nodeID: owner.ID, rank: 0})
}

func TestMapDiscoveredExploreSourceLiteralMatchesPropagatesStickyAdjacencyCancellation(t *testing.T) {
	const path = "demo/src/cancel.cs"
	owner := sourceLiteralNode(path+"::owner", "owner", path, graph.KindMethod, 1, 10)
	callee := sourceLiteralNode(path+"::Resolve", "Resolve", path, graph.KindMethod, 20, 30)
	server, counting := newExploreSourceLiteralGraphServer(t, []*graph.Node{owner, callee}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	counting.siteLookup = func(_ context.Context, sites []graph.EdgeSourceSite, _ []graph.EdgeKind, _ int) (graph.BoundedSiteEdgeIdentityProjection, error) {
		cancel()
		return graph.BoundedSiteEdgeIdentityProjection{BySite: map[graph.EdgeSourceSite][]graph.EdgeIdentity{
			sites[0]: {{From: owner.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: path, Line: 3}},
		}}, nil
	}

	recall, err := server.mapDiscoveredExploreSourceLiteralMatches(ctx, "needle", exploreSourceLiteralSearch{
		matches: []trigram.Match{{Path: path, Line: 3, Text: `Resolve("needle");`}},
	}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}}, nil)
	require.ErrorIs(t, err, context.Canceled)
	requireSourceLiteralHitIdentity(t, recall.hits, exploreSourceLiteralHit{nodeID: owner.ID, rank: 0})
	require.Zero(t, counting.nodeLookupBatches)
}

func TestProjectExploreSourceLiteralConstructionAlignmentBoundsAndFailures(t *testing.T) {
	const (
		path     = "demo/src/construction.cs"
		sourceID = path + "::source"
		targetID = path + "::target"
	)
	source := sourceLiteralNode(sourceID, "source", path, graph.KindMethod, 1, 10)
	target := sourceLiteralNode(targetID, "target", path, graph.KindType, 20, 30)
	edge := graph.EdgeIdentity{From: sourceID, To: targetID, Kind: graph.EdgeInstantiates, FilePath: path, Line: 3}

	t.Run("complete positive", func(t *testing.T) {
		server, counting := newExploreSourceLiteralGraphServer(t, []*graph.Node{source, target}, []*graph.Edge{{
			From: sourceID, To: targetID, Kind: graph.EdgeInstantiates, FilePath: path, Line: 3,
		}})
		aligned := projectExploreSourceLiteralConstructionAlignment(context.Background(), server.graph, []string{sourceID})
		require.Equal(t, map[string]bool{sourceID: true}, aligned)
		require.Equal(t, 1, counting.outEndpointBatchCalls)
		require.Equal(t, []string{sourceID}, counting.lastEndpointIDs)
		require.Equal(t, []graph.EdgeKind{graph.EdgeInstantiates}, counting.lastEndpointKinds)
		require.Equal(t, graph.MaxBoundedAdjacencyRowsPerKey, counting.lastEndpointLimit)
		require.Zero(t, counting.outEdgeBatchCalls)
	})

	for _, test := range []struct {
		name    string
		count   int
		aligned bool
	}{
		{name: "graph exact 256 is complete", count: graph.MaxBoundedAdjacencyRowsPerKey, aligned: true},
		{name: "graph 257th sentinel fails closed", count: graph.MaxBoundedAdjacencyRowsPerKey + 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := graph.New()
			edges := make([]*graph.Edge, 0, test.count)
			for line := 1; line <= test.count; line++ {
				edges = append(edges, &graph.Edge{
					From: sourceID, To: targetID, Kind: graph.EdgeInstantiates,
					FilePath: path, Line: line,
				})
			}
			base.AddBatch([]*graph.Node{source, target}, edges)
			aligned := projectExploreSourceLiteralConstructionAlignment(
				context.Background(), base, []string{sourceID},
			)
			require.Equal(t, test.aligned, aligned[sourceID])
		})
	}

	t.Run("zero", func(t *testing.T) {
		server, counting := newExploreSourceLiteralGraphServer(t, []*graph.Node{source, target}, nil)
		require.Empty(t, projectExploreSourceLiteralConstructionAlignment(context.Background(), server.graph, []string{sourceID}))
		require.Equal(t, 1, counting.outEndpointBatchCalls)
	})

	for _, test := range []struct {
		name       string
		projection graph.BoundedEdgeIdentityProjection
		err        error
	}{
		{name: "truncated is not positive", projection: graph.BoundedEdgeIdentityProjection{
			ByEndpoint: map[string][]graph.EdgeIdentity{sourceID: {edge}},
			Truncated:  map[string]bool{sourceID: true},
		}},
		{name: "partial backend error", projection: graph.BoundedEdgeIdentityProjection{
			ByEndpoint: map[string][]graph.EdgeIdentity{sourceID: {edge}},
		}, err: errors.New("partial backend failure")},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, counting := newExploreSourceLiteralGraphServer(t, []*graph.Node{source, target}, nil)
			counting.endpointLookup = func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error) {
				return test.projection, test.err
			}
			require.Empty(t, projectExploreSourceLiteralConstructionAlignment(context.Background(), server.graph, []string{sourceID}))
		})
	}

	t.Run("sticky cancellation", func(t *testing.T) {
		server, counting := newExploreSourceLiteralGraphServer(t, []*graph.Node{source, target}, nil)
		ctx, cancel := context.WithCancel(context.Background())
		counting.endpointLookup = func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error) {
			cancel()
			return graph.BoundedEdgeIdentityProjection{ByEndpoint: map[string][]graph.EdgeIdentity{sourceID: {edge}}}, nil
		}
		require.Empty(t, projectExploreSourceLiteralConstructionAlignment(ctx, server.graph, []string{sourceID}))
	})

	t.Run("unsupported has no legacy fallback", func(t *testing.T) {
		base := graph.New()
		require.Empty(t, projectExploreSourceLiteralConstructionAlignment(
			context.Background(), &exploreSourceLiteralUnsupportedSiteStore{Store: base}, []string{sourceID},
		))
	})
}

func TestProjectExploreSourceLiteralConstructionAlignmentOverlayParity(t *testing.T) {
	const (
		sourceFile = "demo/source.cs"
		targetFile = "demo/target.cs"
		sourceID   = sourceFile + "::source"
		targetID   = targetFile + "::target"
	)
	base := graph.New()
	base.AddNode(&graph.Node{ID: sourceID, Name: "source", Kind: graph.KindMethod, FilePath: sourceFile})
	base.AddNode(&graph.Node{ID: targetID, Name: "target", Kind: graph.KindType, FilePath: targetFile})
	base.AddEdge(&graph.Edge{From: sourceID, To: targetID, Kind: graph.EdgeInstantiates, FilePath: sourceFile, Line: 4})

	require.True(t, projectExploreSourceLiteralConstructionAlignment(
		context.Background(), base, []string{sourceID},
	)[sourceID])

	t.Run("source tombstone and replacement", func(t *testing.T) {
		tombstone := graph.NewOverlayLayer()
		tombstone.MarkFile(sourceFile, false)
		tombstone.MarkRemoved("source", sourceID)
		tombstone.AddEdge(&graph.Edge{From: sourceID, To: targetID, Kind: graph.EdgeInstantiates, Line: 99})
		require.Empty(t, projectExploreSourceLiteralConstructionAlignment(
			context.Background(), graph.NewOverlaidView(base, tombstone), []string{sourceID},
		))

		replacement := graph.NewOverlayLayer()
		replacement.MarkFile(sourceFile, false)
		replacement.MarkRemoved("source", sourceID)
		replacement.AddNode(sourceFile, &graph.Node{ID: sourceID, Name: "source", Kind: graph.KindMethod, FilePath: sourceFile})
		replacement.AddEdge(&graph.Edge{From: sourceID, To: targetID, Kind: graph.EdgeInstantiates, FilePath: sourceFile, Line: 8})
		require.True(t, projectExploreSourceLiteralConstructionAlignment(
			context.Background(), graph.NewOverlaidView(base, replacement), []string{sourceID},
		)[sourceID])
	})

	t.Run("target tombstone and replacement", func(t *testing.T) {
		tombstone := graph.NewOverlayLayer()
		tombstone.MarkFile(targetFile, false)
		tombstone.MarkRemoved("target", targetID)
		require.Empty(t, projectExploreSourceLiteralConstructionAlignment(
			context.Background(), graph.NewOverlaidView(base, tombstone), []string{sourceID},
		))

		replacement := graph.NewOverlayLayer()
		replacement.MarkFile(targetFile, false)
		replacement.MarkRemoved("target", targetID)
		replacement.AddNode(targetFile, &graph.Node{ID: targetID, Name: "target", Kind: graph.KindType, FilePath: targetFile})
		require.True(t, projectExploreSourceLiteralConstructionAlignment(
			context.Background(), graph.NewOverlaidView(base, replacement), []string{sourceID},
		)[sourceID])
	})
}

func TestProjectExploreSourceLiteralConstructionAlignmentRejectsOverlayHiddenTruncation(t *testing.T) {
	const (
		sourceFile = "demo/source.cs"
		targetFile = "demo/target.cs"
		sourceID   = sourceFile + "::source"
		targetID   = targetFile + "::target"
	)
	base := graph.New()
	base.AddNode(&graph.Node{ID: sourceID, Name: "source", Kind: graph.KindMethod, FilePath: sourceFile})
	base.AddNode(&graph.Node{ID: targetID, Name: "target", Kind: graph.KindType, FilePath: targetFile})
	for line := 1; line <= graph.MaxBoundedAdjacencyRowsPerKey+1; line++ {
		base.AddEdge(&graph.Edge{
			From: sourceID, To: targetID, Kind: graph.EdgeInstantiates,
			FilePath: sourceFile, Line: line,
		})
	}
	layer := graph.NewOverlayLayer()
	layer.MarkFile(targetFile, false)
	layer.MarkRemoved("target", targetID)

	require.Empty(t, projectExploreSourceLiteralConstructionAlignment(
		context.Background(), graph.NewOverlaidView(base, layer), []string{sourceID},
	), "a base truncation hidden by the overlay is indeterminate, never positive")
}

func TestMapExploreSourceLiteralMatchesDedupesProjectedCalleeIdentities(t *testing.T) {
	const path = "demo/src/duplicate.cs"
	owner := sourceLiteralNode(path+"::owner", "owner", path, graph.KindMethod, 1, 10)
	callee := sourceLiteralNode(path+"::Resolve", "Resolve", path, graph.KindMethod, 20, 30)
	server, counting := newExploreSourceLiteralGraphServer(t, []*graph.Node{owner, callee}, nil)
	counting.siteLookup = func(_ context.Context, sites []graph.EdgeSourceSite, _ []graph.EdgeKind, _ int) (graph.BoundedSiteEdgeIdentityProjection, error) {
		identity := graph.EdgeIdentity{From: owner.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: path, Line: 3}
		return graph.BoundedSiteEdgeIdentityProjection{BySite: map[graph.EdgeSourceSite][]graph.EdgeIdentity{
			sites[0]: {identity, identity},
		}}, nil
	}

	recall := server.mapExploreSourceLiteralMatches("needle", []trigram.Match{{
		Path: path, Line: 3, Text: `Resolve("needle");`,
	}}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})
	requireSourceLiteralHitIdentity(t, recall.hits, exploreSourceLiteralHit{nodeID: callee.ID, rank: 0, callee: true})
	require.Equal(t, []string{callee.ID}, counting.lastNodeIDs)
	require.Zero(t, counting.outEdgeBatchCalls)
}

func TestMapExploreSourceLiteralMatchesOverlayAdjacencyParity(t *testing.T) {
	const (
		sourceFile = "demo/source.cs"
		targetFile = "demo/target.cs"
		sourceID   = sourceFile + "::owner"
		targetID   = targetFile + "::Resolve"
	)
	newFixture := func(t *testing.T, layer *graph.OverlayLayer) (*Server, *graph.OverlaidView) {
		t.Helper()
		owner := sourceLiteralNode(sourceID, "owner", sourceFile, graph.KindMethod, 1, 10)
		callee := sourceLiteralNode(targetID, "Resolve", targetFile, graph.KindMethod, 20, 30)
		server, counting := newExploreSourceLiteralGraphServer(t, []*graph.Node{owner, callee}, []*graph.Edge{{
			From: sourceID, To: targetID, Kind: graph.EdgeCalls, FilePath: sourceFile, Line: 3,
		}})
		return server, graph.NewOverlaidView(counting.Store, layer)
	}
	match := []trigram.Match{{Path: sourceFile, Line: 3, Text: `Resolve("needle");`}}
	scope := query.QueryOptions{RepoAllow: map[string]bool{"demo": true}}
	mapWithView := func(server *Server, view *graph.OverlaidView) exploreSourceLiteralRecall {
		return server.mapExploreSourceLiteralMatchesContext(
			WithOverlayView(context.Background(), view), "needle", match, scope,
		)
	}

	t.Run("source tombstone and replacement", func(t *testing.T) {
		tombstone := graph.NewOverlayLayer()
		tombstone.MarkFile(sourceFile, false)
		tombstone.MarkRemoved("owner", sourceID)
		tombstone.AddEdge(&graph.Edge{From: sourceID, To: targetID, Kind: graph.EdgeCalls, Line: 3})
		server, view := newFixture(t, tombstone)
		require.Empty(t, mapWithView(server, view).hits)

		replacement := graph.NewOverlayLayer()
		replacement.MarkFile(sourceFile, false)
		replacement.MarkRemoved("owner", sourceID)
		replacement.AddNode(sourceFile, sourceLiteralNode(sourceID, "owner", sourceFile, graph.KindMethod, 1, 10))
		replacement.AddEdge(&graph.Edge{
			From: sourceID, To: targetID, Kind: graph.EdgeCalls, FilePath: sourceFile, Line: 3,
		})
		server, view = newFixture(t, replacement)
		recall := mapWithView(server, view)
		requireSourceLiteralHitIdentity(t, recall.hits, exploreSourceLiteralHit{nodeID: targetID, rank: 0, callee: true})
	})

	t.Run("target tombstone and replacement", func(t *testing.T) {
		tombstone := graph.NewOverlayLayer()
		tombstone.MarkFile(targetFile, false)
		tombstone.MarkRemoved("Resolve", targetID)
		server, view := newFixture(t, tombstone)
		recall := mapWithView(server, view)
		requireSourceLiteralHitIdentity(t, recall.hits, exploreSourceLiteralHit{nodeID: sourceID, rank: 0})

		replacement := graph.NewOverlayLayer()
		replacement.MarkFile(targetFile, false)
		replacement.MarkRemoved("Resolve", targetID)
		replacement.AddNode(targetFile, sourceLiteralNode(targetID, "Resolve", targetFile, graph.KindMethod, 20, 30))
		server, view = newFixture(t, replacement)
		recall = mapWithView(server, view)
		requireSourceLiteralHitIdentity(t, recall.hits, exploreSourceLiteralHit{nodeID: targetID, rank: 0, callee: true})
	})
}

func TestMapExploreSourceLiteralMatchesFiltersCalleeScopeAndTests(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*graph.Node, *graph.Node, *query.QueryOptions)
	}{
		{name: "workspace", configure: func(owner, callee *graph.Node, scope *query.QueryOptions) {
			owner.WorkspaceID = "demo"
			callee.WorkspaceID = "other"
			scope.WorkspaceID = "demo"
		}},
		{name: "test", configure: func(_ *graph.Node, callee *graph.Node, scope *query.QueryOptions) {
			callee.Meta = map[string]any{"is_test": true}
			scope.ExcludeTests = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			const path = "demo/src/scope.cs"
			owner := sourceLiteralNode(path+"::owner", "owner", path, graph.KindMethod, 1, 10)
			callee := sourceLiteralNode(path+"::Resolve", "Resolve", path, graph.KindMethod, 20, 30)
			scope := query.QueryOptions{RepoAllow: map[string]bool{"demo": true}}
			test.configure(owner, callee, &scope)
			server, _ := newExploreSourceLiteralGraphServer(t, []*graph.Node{owner, callee}, []*graph.Edge{{
				From: owner.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: path, Line: 3,
			}})
			recall := server.mapExploreSourceLiteralMatches("needle", []trigram.Match{{
				Path: path, Line: 3, Text: `Resolve("needle");`,
			}}, scope)
			requireSourceLiteralHitIdentity(t, recall.hits, exploreSourceLiteralHit{nodeID: owner.ID, rank: 0})
		})
	}
}

func TestMapExploreSourceLiteralMatchesDiscardsPartialSiteError(t *testing.T) {
	const path = "demo/src/error.cs"
	owner := sourceLiteralNode(path+"::owner", "owner", path, graph.KindMethod, 1, 10)
	callee := sourceLiteralNode(path+"::Resolve", "Resolve", path, graph.KindMethod, 20, 30)
	server, counting := newExploreSourceLiteralGraphServer(t, []*graph.Node{owner, callee}, nil)
	counting.siteLookup = func(_ context.Context, sites []graph.EdgeSourceSite, _ []graph.EdgeKind, _ int) (graph.BoundedSiteEdgeIdentityProjection, error) {
		return graph.BoundedSiteEdgeIdentityProjection{BySite: map[graph.EdgeSourceSite][]graph.EdgeIdentity{
			sites[0]: {{From: owner.ID, To: callee.ID, Kind: graph.EdgeCalls, FilePath: path, Line: 3}},
		}}, errors.New("backend failed after a partial page")
	}

	recall := server.mapExploreSourceLiteralMatches("needle", []trigram.Match{{
		Path: path, Line: 3, Text: `Resolve("needle");`,
	}}, query.QueryOptions{RepoAllow: map[string]bool{"demo": true}})
	requireSourceLiteralHitIdentity(t, recall.hits, exploreSourceLiteralHit{nodeID: owner.ID, rank: 0})
	require.Zero(t, counting.nodeLookupBatches)
}
