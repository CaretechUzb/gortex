package mcp

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func exploreOwnerFoldingOverlayFolds(
	ctx context.Context,
	server *Server,
	fixture exploreImplementationStoreFixture,
) bool {
	folded := server.foldMemberOwners(ctx, []exploreTarget{
		{node: fixture.foldA, score: 1},
		{node: fixture.foldB, score: .9},
	}, graph.LocalizationNodeScope{})
	return len(folded) == 3 && folded[0].node != nil && folded[0].node.ID == fixture.foldOwner.ID
}

func TestOwnerFoldingUsesEngineOverlayReader(t *testing.T) {
	fixture := newExploreImplementationStoreFixture()
	routeStore := graph.New()
	addExploreImplementationFixtureToStore(routeStore, fixture)
	server := &Server{graph: graph.New(), engine: query.NewEngine(routeStore)}
	if !exploreOwnerFoldingOverlayFolds(context.Background(), server, fixture) {
		t.Fatal("base engine reader lost its independently configured graph")
	}

	tests := []struct {
		name string
		want bool
		make func() *graph.OverlayLayer
	}{
		{
			name: "detached source tombstone",
			make: func() *graph.OverlayLayer {
				layer := graph.NewOverlayLayer()
				layer.MarkRemoved(fixture.foldA.Name, fixture.foldA.ID)
				return layer
			},
		},
		{
			name: "detached source same-ID replacement",
			want: true,
			make: func() *graph.OverlayLayer {
				layer := graph.NewOverlayLayer()
				layer.MarkRemoved(fixture.foldA.Name, fixture.foldA.ID)
				layer.AddNode(fixture.foldA.FilePath, fixture.foldA)
				layer.AddEdge(&graph.Edge{From: fixture.foldA.ID, To: fixture.foldOwner.ID, Kind: graph.EdgeMemberOf})
				return layer
			},
		},
		{
			name: "detached target tombstone",
			make: func() *graph.OverlayLayer {
				layer := graph.NewOverlayLayer()
				layer.MarkRemoved(fixture.foldOwner.Name, fixture.foldOwner.ID)
				return layer
			},
		},
		{
			name: "detached target same-ID replacement",
			want: true,
			make: func() *graph.OverlayLayer {
				layer := graph.NewOverlayLayer()
				layer.MarkRemoved(fixture.foldOwner.Name, fixture.foldOwner.ID)
				layer.AddNode(fixture.foldOwner.FilePath, fixture.foldOwner)
				return layer
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			view := graph.NewOverlaidView(routeStore, test.make())
			ctx := WithOverlayView(context.Background(), view)
			eng := server.engineFor(ctx)
			if eng == nil || eng.Reader() != view {
				t.Fatal("engineFor did not bind the request-local overlay reader")
			}
			if got := exploreOwnerFoldingOverlayFolds(ctx, server, fixture); got != test.want {
				t.Fatalf("folded=%v, want %v", got, test.want)
			}
		})
	}
	if !exploreOwnerFoldingOverlayFolds(context.Background(), server, fixture) {
		t.Fatal("overlay requests mutated the base engine reader")
	}
}
