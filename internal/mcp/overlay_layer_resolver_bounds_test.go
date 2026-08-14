package mcp

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func overlayLayerWithUnresolved(names ...string) (*graph.OverlayLayer, []string) {
	layer := graph.NewOverlayLayer()
	layer.MarkFile("overlay.go", false)
	fromIDs := make([]string, 0, len(names))
	for index, name := range names {
		from := fmt.Sprintf("overlay.go::caller-%d", index)
		fromIDs = append(fromIDs, from)
		layer.AddEdge(&graph.Edge{From: from, To: unresolvedPrefix + "call::" + name})
	}
	return layer, fromIDs
}

func TestResolveOverlayEdgesDedupesSortsAndRebuildsInbound(t *testing.T) {
	layer, fromIDs := overlayLayerWithUnresolved("Beta", "Alpha", "Alpha")
	store := &overlayLayerTestStore{Store: graph.New()}
	store.nameFn = func(
		_ context.Context,
		name string,
		_ graph.LocalizationNodeScope,
		limit int,
	) (graph.BoundedNodeProjection, error) {
		require.Equal(t, 1, limit)
		return graph.BoundedNodeProjection{
			Nodes: []*graph.Node{{ID: "base.go::" + name, Name: name, FilePath: "base.go"}},
			Total: 1,
		}, nil
	}

	require.NoError(t, resolveOverlayEdges(context.Background(), store, layer))
	calls := store.snapshotNameCalls()
	_, legacyNameReads := store.snapshotLegacyReads()
	require.Zero(t, legacyNameReads, "resolver must never fall back to FindNodesByName")
	require.Equal(t, []string{"Alpha", "Beta"}, []string{calls[0].name, calls[1].name})
	for _, call := range calls {
		require.Equal(t, 1, call.limit)
	}

	view := graph.NewOverlaidView(store, layer)
	require.Equal(t, "base.go::Beta", view.GetOutEdges(fromIDs[0])[0].To)
	require.Equal(t, "base.go::Alpha", view.GetOutEdges(fromIDs[1])[0].To)
	require.Equal(t, "base.go::Alpha", view.GetOutEdges(fromIDs[2])[0].To)
	require.Len(t, view.GetInEdges("base.go::Alpha"), 2)
}

func TestResolveOverlayEdgesDistinctNameBounds(t *testing.T) {
	names := make([]string, overlayLayerUnresolvedNamesMax)
	for index := range names {
		names[index] = fmt.Sprintf("Name%04d", index)
	}
	layer, _ := overlayLayerWithUnresolved(names...)
	store := &overlayLayerTestStore{Store: graph.New()}
	store.nameFn = func(
		context.Context, string, graph.LocalizationNodeScope, int,
	) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{}, nil
	}
	require.NoError(t, resolveOverlayEdges(context.Background(), store, layer))
	calls := store.snapshotNameCalls()
	require.Len(t, calls, overlayLayerUnresolvedNamesMax)
	ordered := make([]string, 0, len(calls))
	for _, call := range calls {
		ordered = append(ordered, call.name)
	}
	require.True(t, sort.StringsAreSorted(ordered))

	names = append(names, "Name-overflow")
	layer, _ = overlayLayerWithUnresolved(names...)
	store = &overlayLayerTestStore{Store: graph.New()}
	_, called := store.nameFn, false
	store.nameFn = func(
		context.Context, string, graph.LocalizationNodeScope, int,
	) (graph.BoundedNodeProjection, error) {
		called = true
		return graph.BoundedNodeProjection{}, nil
	}
	err := resolveOverlayEdges(context.Background(), store, layer)
	var limitErr *overlayLayerLimitError
	require.ErrorAs(t, err, &limitErr)
	require.Equal(t, "unresolved names", limitErr.Resource)
	require.False(t, called, "the complete distinct-name set must be capped before querying base")
}

func TestResolveOverlayEdgesOverlayPrecedence(t *testing.T) {
	t.Run("unique", func(t *testing.T) {
		layer, fromIDs := overlayLayerWithUnresolved("Target")
		layer.AddNode("overlay.go", &graph.Node{ID: "overlay.go::Target", Name: "Target", FilePath: "overlay.go"})
		store := &overlayLayerTestStore{Store: graph.New()}
		store.nameFn = func(
			context.Context, string, graph.LocalizationNodeScope, int,
		) (graph.BoundedNodeProjection, error) {
			return graph.BoundedNodeProjection{}, errors.New("base must not be queried")
		}
		require.NoError(t, resolveOverlayEdges(context.Background(), store, layer))
		require.Empty(t, store.snapshotNameCalls())
		require.Equal(t, "overlay.go::Target", graph.NewOverlaidView(store, layer).GetOutEdges(fromIDs[0])[0].To)
	})

	t.Run("ambiguous", func(t *testing.T) {
		layer, fromIDs := overlayLayerWithUnresolved("Target")
		layer.AddNode("overlay.go", &graph.Node{ID: "overlay.go::Target-one", Name: "Target", FilePath: "overlay.go"})
		layer.AddNode("overlay.go", &graph.Node{ID: "overlay.go::Target-two", Name: "Target", FilePath: "overlay.go"})
		store := &overlayLayerTestStore{Store: graph.New()}
		store.nameFn = func(
			context.Context, string, graph.LocalizationNodeScope, int,
		) (graph.BoundedNodeProjection, error) {
			return graph.BoundedNodeProjection{}, errors.New("base must not be queried")
		}
		require.NoError(t, resolveOverlayEdges(context.Background(), store, layer))
		require.Empty(t, store.snapshotNameCalls())
		require.Equal(t, unresolvedPrefix+"call::Target", graph.NewOverlaidView(store, layer).GetOutEdges(fromIDs[0])[0].To)
	})
}

func TestResolveOverlayEdgesBaseCardinality(t *testing.T) {
	for _, test := range []struct {
		name       string
		projection graph.BoundedNodeProjection
		wantTarget string
	}{
		{name: "zero", projection: graph.BoundedNodeProjection{}, wantTarget: unresolvedPrefix + "call::Target"},
		{
			name: "one",
			projection: graph.BoundedNodeProjection{
				Nodes: []*graph.Node{{ID: "base.go::Target", Name: "Target", FilePath: "base.go"}},
				Total: 1,
			},
			wantTarget: "base.go::Target",
		},
		{
			name: "ambiguous",
			projection: graph.BoundedNodeProjection{
				Nodes:     []*graph.Node{{ID: "base.go::Target", Name: "Target", FilePath: "base.go"}},
				Total:     2,
				Truncated: true,
			},
			wantTarget: unresolvedPrefix + "call::Target",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			layer, fromIDs := overlayLayerWithUnresolved("Target")
			store := &overlayLayerTestStore{Store: graph.New()}
			store.nameFn = func(
				context.Context, string, graph.LocalizationNodeScope, int,
			) (graph.BoundedNodeProjection, error) {
				return test.projection, nil
			}
			require.NoError(t, resolveOverlayEdges(context.Background(), store, layer))
			require.Equal(t, test.wantTarget, graph.NewOverlaidView(store, layer).GetOutEdges(fromIDs[0])[0].To)
		})
	}
}

func TestResolveOverlayEdgesFiltersCoveredBaseFiles(t *testing.T) {
	for _, tombstone := range []bool{false, true} {
		name := "replacement"
		if tombstone {
			name = "tombstone"
		}
		t.Run(name, func(t *testing.T) {
			layer := graph.NewOverlayLayer()
			layer.MarkFile("base.go", tombstone)
			layer.MarkFile("overlay.go", false)
			from := "overlay.go::caller"
			layer.AddEdge(&graph.Edge{From: from, To: unresolvedPrefix + "call::Stale"})
			store := &overlayLayerTestStore{Store: graph.New()}
			store.nameFn = func(
				_ context.Context,
				_ string,
				scope graph.LocalizationNodeScope,
				_ int,
			) (graph.BoundedNodeProjection, error) {
				require.True(t, scope.ExcludesFile("base.go"))
				return graph.BoundedNodeProjection{}, nil
			}
			require.NoError(t, resolveOverlayEdges(context.Background(), store, layer))
			require.Equal(t, unresolvedPrefix+"call::Stale", graph.NewOverlaidView(store, layer).GetOutEdges(from)[0].To)
		})
	}
}

func TestResolveOverlayEdgesFailsClosedWithoutPartialRewrite(t *testing.T) {
	sentinel := errors.New("query failed")
	layer, fromIDs := overlayLayerWithUnresolved("Alpha", "Beta")
	store := &overlayLayerTestStore{Store: graph.New()}
	store.nameFn = func(
		_ context.Context,
		name string,
		_ graph.LocalizationNodeScope,
		_ int,
	) (graph.BoundedNodeProjection, error) {
		if name == "Beta" {
			return graph.BoundedNodeProjection{}, sentinel
		}
		return graph.BoundedNodeProjection{
			Nodes: []*graph.Node{{ID: "base.go::Alpha", Name: "Alpha", FilePath: "base.go"}},
			Total: 1,
		}, nil
	}
	err := resolveOverlayEdges(context.Background(), store, layer)
	require.ErrorIs(t, err, sentinel)
	view := graph.NewOverlaidView(store, layer)
	require.Equal(t, unresolvedPrefix+"call::Alpha", view.GetOutEdges(fromIDs[0])[0].To)
	require.Equal(t, unresolvedPrefix+"call::Beta", view.GetOutEdges(fromIDs[1])[0].To)

	legacy := &overlayLayerLegacyStore{Store: graph.New()}
	unsupportedLayer, _ := overlayLayerWithUnresolved("Target")
	err = resolveOverlayEdges(context.Background(), legacy, unsupportedLayer)
	require.ErrorIs(t, err, graph.ErrBoundedLocalizationUnavailable)
}

func TestResolveOverlayEdgesPropagatesCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	layer, fromIDs := overlayLayerWithUnresolved("Target")
	store := &overlayLayerTestStore{Store: graph.New()}
	store.nameFn = func(
		context.Context, string, graph.LocalizationNodeScope, int,
	) (graph.BoundedNodeProjection, error) {
		cancel()
		return graph.BoundedNodeProjection{}, nil
	}
	err := resolveOverlayEdges(ctx, store, layer)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, unresolvedPrefix+"call::Target", graph.NewOverlaidView(store, layer).GetOutEdges(fromIDs[0])[0].To)
}
