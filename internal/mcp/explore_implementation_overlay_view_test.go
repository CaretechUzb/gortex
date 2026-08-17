package mcp

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func exploreImplementationOverlayExpands(
	ctx context.Context,
	server *Server,
	fixture exploreImplementationStoreFixture,
) bool {
	expanded := server.expandImplementationTargets(
		ctx, []exploreTarget{{node: fixture.seed, score: 1}}, graph.LocalizationNodeScope{},
	)
	return len(expanded) == 2 && expanded[1].node != nil && expanded[1].node.ID == fixture.member.ID
}

func TestImplementationExpansionUsesEngineOverlayReader(t *testing.T) {
	fixture := newExploreImplementationStoreFixture()
	routeStore := graph.New()
	addExploreImplementationFixtureToStore(routeStore, fixture)
	server := &Server{graph: graph.New(), engine: query.NewEngine(routeStore)}
	if !exploreImplementationOverlayExpands(context.Background(), server, fixture) {
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
				layer.MarkRemoved(fixture.seed.Name, fixture.seed.ID)
				return layer
			},
		},
		{
			name: "detached source same-ID replacement",
			want: true,
			make: func() *graph.OverlayLayer {
				layer := graph.NewOverlayLayer()
				layer.MarkRemoved(fixture.seed.Name, fixture.seed.ID)
				layer.AddNode(fixture.seed.FilePath, fixture.seed)
				layer.AddEdge(&graph.Edge{From: fixture.seed.ID, To: fixture.owner.ID, Kind: graph.EdgeMemberOf})
				return layer
			},
		},
		{
			name: "detached target tombstone",
			make: func() *graph.OverlayLayer {
				layer := graph.NewOverlayLayer()
				layer.MarkRemoved(fixture.owner.Name, fixture.owner.ID)
				return layer
			},
		},
		{
			name: "detached target same-ID replacement",
			want: true,
			make: func() *graph.OverlayLayer {
				layer := graph.NewOverlayLayer()
				layer.MarkRemoved(fixture.owner.Name, fixture.owner.ID)
				layer.AddNode(fixture.owner.FilePath, fixture.owner)
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
			if got := exploreImplementationOverlayExpands(ctx, server, fixture); got != test.want {
				t.Fatalf("expanded=%v, want %v", got, test.want)
			}
		})
	}
	if !exploreImplementationOverlayExpands(context.Background(), server, fixture) {
		t.Fatal("overlay requests mutated the base engine reader")
	}
}
