package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/trigram"
)

func TestOverlayLocalizationFiltersShadowedDurableContent(t *testing.T) {
	base := graph.New()
	for _, deleted := range []bool{false, true} {
		layer := graph.NewOverlayLayer()
		layer.MarkFile("shadow.go", deleted)
		ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))
		srv := &Server{graph: base}
		hits := []graph.ContentHit{
			{NodeID: "shadow.go::stale", FilePath: "shadow.go", Snippet: "stale needle"},
			{NodeID: "live.go::fresh", FilePath: "live.go", Snippet: "fresh needle"},
		}
		filtered := srv.filterOverlayContentHits(ctx, hits)
		require.Equal(t, []graph.ContentHit{hits[1]}, filtered)
	}
}

func TestOverlayLocalizationMapsReplacementOwnerAndAdjacency(t *testing.T) {
	base := graph.New()
	baseOwner := &graph.Node{ID: "sample.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "sample.go", StartLine: 1, EndLine: 5}
	staleCallee := &graph.Node{ID: "sample.go::Stale", Kind: graph.KindFunction, Name: "Stale", FilePath: "sample.go", StartLine: 7, EndLine: 9}
	base.AddBatch([]*graph.Node{baseOwner, staleCallee}, []*graph.Edge{{From: baseOwner.ID, To: staleCallee.ID, Kind: graph.EdgeCalls, Line: 3}})

	layer := graph.NewOverlayLayer()
	layer.MarkFile("sample.go", false)
	overlayOwner := &graph.Node{ID: baseOwner.ID, Kind: graph.KindFunction, Name: "Caller", FilePath: "sample.go", StartLine: 1, EndLine: 5}
	freshCallee := &graph.Node{ID: "sample.go::Fresh", Kind: graph.KindFunction, Name: "Fresh", FilePath: "sample.go", StartLine: 7, EndLine: 9}
	layer.AddNode("sample.go", overlayOwner)
	layer.AddNode("sample.go", freshCallee)
	layer.AddEdge(&graph.Edge{From: overlayOwner.ID, To: freshCallee.ID, Kind: graph.EdgeCalls, Line: 3})
	view := graph.NewOverlaidView(base, layer)
	ctx := WithOverlayView(context.Background(), view)

	srv := &Server{graph: base}
	recall := srv.mapExploreSourceLiteralMatchesContext(ctx, "needle", []trigram.Match{{
		Path: "sample.go", Line: 3, Text: `Fresh("needle")`,
	}}, query.QueryOptions{})
	require.Len(t, recall.hits, 1)
	require.Equal(t, freshCallee.ID, recall.hits[0].nodeID)
	require.True(t, recall.hits[0].callee)
}

func TestOverlayLocalizationTombstoneSuppressesSourceOwner(t *testing.T) {
	base := graph.New()
	owner := &graph.Node{ID: "sample.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "sample.go", StartLine: 1, EndLine: 5}
	base.AddNode(owner)
	layer := graph.NewOverlayLayer()
	layer.MarkFile("sample.go", true)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))

	srv := &Server{graph: base}
	recall := srv.mapExploreSourceLiteralMatchesContext(ctx, "needle", []trigram.Match{{
		Path: "sample.go", Line: 3, Text: `Call("needle")`,
	}}, query.QueryOptions{})
	require.Empty(t, recall.hits)
}

func TestOverlayLocalizationQualifiedOwnerUsesReplacement(t *testing.T) {
	base := graph.New()
	staleOwner := &graph.Node{ID: "sample.go::Owner", Kind: graph.KindType, Name: "Owner", FilePath: "sample.go", StartLine: 1, EndLine: 8}
	base.AddNode(staleOwner)

	layer := graph.NewOverlayLayer()
	layer.MarkFile("sample.go", false)
	layer.MarkRemoved(staleOwner.Name, staleOwner.ID)
	freshOwner := &graph.Node{ID: "sample.go::OwnerV2", Kind: graph.KindType, Name: "Owner", FilePath: "sample.go", StartLine: 1, EndLine: 10}
	member := &graph.Node{ID: "sample.go::Owner.run", Kind: graph.KindMethod, Name: "run", FilePath: "sample.go", StartLine: 3, EndLine: 5}
	layer.AddNode("sample.go", freshOwner)
	layer.AddNode("sample.go", member)
	ctx := WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))

	srv := &Server{graph: base}
	candidate := srv.exploreQualifiedAnchorOwnerCandidate(ctx, exploreSyntacticAnchor{
		qualifiedName: "Owner.run",
	}, member, query.QueryOptions{}, map[string]struct{}{})
	require.NotNil(t, candidate)
	require.Equal(t, freshOwner.ID, candidate.Node.ID)
}
