package mcp

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func exploreImplementationOverlayExpandedID(ctx context.Context, server *Server, seed *graph.Node) string {
	expanded := server.expandImplementationTargets(
		ctx, []exploreTarget{{node: seed, score: 1}}, graph.LocalizationNodeScope{},
	)
	if len(expanded) != 2 || expanded[1].node == nil {
		return ""
	}
	return expanded[1].node.ID
}

func TestImplementationExpansionOverlayIncomingStages(t *testing.T) {
	fixture := newExploreImplementationStoreFixture()
	base := graph.New()
	addExploreImplementationFixtureToStore(base, fixture)
	server := &Server{engine: query.NewEngine(base)}
	withLayer := func(layer *graph.OverlayLayer) context.Context {
		return WithOverlayView(context.Background(), graph.NewOverlaidView(base, layer))
	}

	t.Run("implementor source", func(t *testing.T) {
		tombstone := graph.NewOverlayLayer()
		tombstone.MarkRemoved(fixture.impl.Name, fixture.impl.ID)
		if got := exploreImplementationOverlayExpandedID(withLayer(tombstone), server, fixture.seed); got != "" {
			t.Fatalf("tombstoned implementor source produced %q", got)
		}

		replacement := graph.NewOverlayLayer()
		replacement.MarkRemoved(fixture.impl.Name, fixture.impl.ID)
		replacement.AddNode(fixture.impl.FilePath, fixture.impl)
		replacement.AddEdge(&graph.Edge{From: fixture.impl.ID, To: fixture.owner.ID, Kind: graph.EdgeImplements})
		if got := exploreImplementationOverlayExpandedID(withLayer(replacement), server, fixture.seed); got != fixture.member.ID {
			t.Fatalf("same-ID implementor source replacement produced %q", got)
		}
	})

	t.Run("implementor target", func(t *testing.T) {
		tombstone := graph.NewOverlayLayer()
		tombstone.MarkRemoved(fixture.owner.Name, fixture.owner.ID)
		if got := exploreImplementationOverlayExpandedID(withLayer(tombstone), server, fixture.owner); got != "" {
			t.Fatalf("tombstoned implementor target produced %q", got)
		}

		replacement := graph.NewOverlayLayer()
		replacement.MarkRemoved(fixture.owner.Name, fixture.owner.ID)
		replacement.AddNode(fixture.owner.FilePath, fixture.owner)
		if got := exploreImplementationOverlayExpandedID(withLayer(replacement), server, fixture.owner); got != fixture.impl.ID {
			t.Fatalf("same-ID implementor target replacement produced %q", got)
		}
	})

	t.Run("member source", func(t *testing.T) {
		tombstone := graph.NewOverlayLayer()
		tombstone.MarkRemoved(fixture.member.Name, fixture.member.ID)
		if got := exploreImplementationOverlayExpandedID(withLayer(tombstone), server, fixture.seed); got != fixture.impl.ID {
			t.Fatalf("tombstoned member source produced %q, want type fallback", got)
		}

		replacement := graph.NewOverlayLayer()
		replacement.MarkRemoved(fixture.member.Name, fixture.member.ID)
		replacement.AddNode(fixture.member.FilePath, fixture.member)
		replacement.AddEdge(&graph.Edge{From: fixture.member.ID, To: fixture.impl.ID, Kind: graph.EdgeMemberOf})
		if got := exploreImplementationOverlayExpandedID(withLayer(replacement), server, fixture.seed); got != fixture.member.ID {
			t.Fatalf("same-ID member source replacement produced %q", got)
		}
	})

	t.Run("member target", func(t *testing.T) {
		tombstone := graph.NewOverlayLayer()
		tombstone.MarkRemoved(fixture.impl.Name, fixture.impl.ID)
		if got := exploreImplementationOverlayExpandedID(withLayer(tombstone), server, fixture.seed); got != "" {
			t.Fatalf("tombstoned member target produced %q", got)
		}

		replacement := graph.NewOverlayLayer()
		replacement.MarkRemoved(fixture.impl.Name, fixture.impl.ID)
		replacement.AddNode(fixture.impl.FilePath, fixture.impl)
		replacement.AddEdge(&graph.Edge{From: fixture.impl.ID, To: fixture.owner.ID, Kind: graph.EdgeImplements})
		if got := exploreImplementationOverlayExpandedID(withLayer(replacement), server, fixture.seed); got != fixture.member.ID {
			t.Fatalf("same-ID member target replacement produced %q", got)
		}
	})
}
