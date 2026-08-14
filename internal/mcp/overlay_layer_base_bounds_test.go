package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type overlayLayerFileReaderFunc func(
	context.Context,
	string,
	graph.LocalizationNodeScope,
	int,
) (graph.BoundedNodeProjection, error)

func (f overlayLayerFileReaderFunc) FindFileNodesBounded(
	ctx context.Context,
	filePath string,
	scope graph.LocalizationNodeScope,
	limit int,
) (graph.BoundedNodeProjection, error) {
	return f(ctx, filePath, scope, limit)
}

func overlayLayerProjection(count, limit int, filePath string) graph.BoundedNodeProjection {
	kept := count
	if kept > limit {
		kept = limit
	}
	nodes := make([]*graph.Node, kept)
	for index := range nodes {
		nodes[index] = &graph.Node{
			ID:       fmt.Sprintf("%s::node-%d", filePath, index),
			Name:     fmt.Sprintf("node-%d", index),
			FilePath: filePath,
		}
	}
	total := count
	if total > limit+1 {
		total = limit + 1
	}
	return graph.BoundedNodeProjection{
		Nodes:     nodes,
		Total:     total,
		Truncated: count > limit,
	}
}

func TestReadOverlayBaseNodesPerFileBounds(t *testing.T) {
	t.Run("exact", func(t *testing.T) {
		total := 0
		reader := overlayLayerFileReaderFunc(func(
			_ context.Context,
			filePath string,
			scope graph.LocalizationNodeScope,
			limit int,
		) (graph.BoundedNodeProjection, error) {
			require.Equal(t, graph.LocalizationNodeScope{}, scope)
			require.Equal(t, overlayLayerBaseNodesPerFileMax, limit)
			return overlayLayerProjection(overlayLayerBaseNodesPerFileMax, limit, filePath), nil
		})

		nodes, err := readOverlayBaseNodes(context.Background(), reader, "exact.go", &total)
		require.NoError(t, err)
		require.Len(t, nodes, overlayLayerBaseNodesPerFileMax)
		require.Equal(t, overlayLayerBaseNodesPerFileMax, total)
	})

	t.Run("plus_one", func(t *testing.T) {
		total := 0
		reader := overlayLayerFileReaderFunc(func(
			_ context.Context,
			filePath string,
			_ graph.LocalizationNodeScope,
			limit int,
		) (graph.BoundedNodeProjection, error) {
			return overlayLayerProjection(overlayLayerBaseNodesPerFileMax+1, limit, filePath), nil
		})

		_, err := readOverlayBaseNodes(context.Background(), reader, "overflow.go", &total)
		var limitErr *overlayLayerLimitError
		require.ErrorAs(t, err, &limitErr)
		require.Equal(t, "base nodes per file", limitErr.Resource)
		require.Zero(t, total)
	})
}

func TestReadOverlayBaseNodesAggregateSentinel(t *testing.T) {
	counts := map[string]int{
		"one.go": 1024, "two.go": 1024, "three.go": 1024, "four.go": 1024,
		"empty.go": 0, "one-more.go": 1,
	}
	var limits []int
	reader := overlayLayerFileReaderFunc(func(
		_ context.Context,
		filePath string,
		_ graph.LocalizationNodeScope,
		limit int,
	) (graph.BoundedNodeProjection, error) {
		limits = append(limits, limit)
		return overlayLayerProjection(counts[filePath], limit, filePath), nil
	})

	total := 0
	for _, filePath := range []string{"one.go", "two.go", "three.go", "four.go"} {
		_, err := readOverlayBaseNodes(context.Background(), reader, filePath, &total)
		require.NoError(t, err)
	}
	require.Equal(t, overlayLayerBaseNodesMax, total)

	nodes, err := readOverlayBaseNodes(context.Background(), reader, "empty.go", &total)
	require.NoError(t, err)
	require.Empty(t, nodes)
	require.Equal(t, 1, limits[len(limits)-1], "full budget must use a non-zero emptiness sentinel")
	require.Equal(t, overlayLayerBaseNodesMax, total)

	_, err = readOverlayBaseNodes(context.Background(), reader, "one-more.go", &total)
	var limitErr *overlayLayerLimitError
	require.ErrorAs(t, err, &limitErr)
	require.Equal(t, "base nodes", limitErr.Resource)
	require.Equal(t, overlayLayerBaseNodesMax, total, "rejection must not retain the sentinel row")
	require.Equal(t, 1, limits[len(limits)-1])
}

func TestReadOverlayBaseNodesRemainingAggregateBudget(t *testing.T) {
	t.Run("exact_remaining", func(t *testing.T) {
		total := 4000
		reader := overlayLayerFileReaderFunc(func(
			_ context.Context,
			filePath string,
			_ graph.LocalizationNodeScope,
			limit int,
		) (graph.BoundedNodeProjection, error) {
			require.Equal(t, 96, limit)
			return overlayLayerProjection(96, limit, filePath), nil
		})
		_, err := readOverlayBaseNodes(context.Background(), reader, "remaining.go", &total)
		require.NoError(t, err)
		require.Equal(t, overlayLayerBaseNodesMax, total)
	})

	t.Run("plus_one_remaining", func(t *testing.T) {
		total := 4000
		reader := overlayLayerFileReaderFunc(func(
			_ context.Context,
			filePath string,
			_ graph.LocalizationNodeScope,
			limit int,
		) (graph.BoundedNodeProjection, error) {
			require.Equal(t, 96, limit)
			return overlayLayerProjection(97, limit, filePath), nil
		})
		_, err := readOverlayBaseNodes(context.Background(), reader, "overflow.go", &total)
		var limitErr *overlayLayerLimitError
		require.ErrorAs(t, err, &limitErr)
		require.Equal(t, "base nodes", limitErr.Resource)
		require.Equal(t, 4000, total)
	})
}

func TestReadOverlayBaseNodesFailsClosed(t *testing.T) {
	t.Run("unsupported", func(t *testing.T) {
		total := 0
		_, err := readOverlayBaseNodes(context.Background(), nil, "file.go", &total)
		require.ErrorIs(t, err, graph.ErrBoundedLocalizationUnavailable)
	})

	t.Run("reader_error", func(t *testing.T) {
		sentinel := errors.New("read failed")
		total := 0
		reader := overlayLayerFileReaderFunc(func(
			context.Context, string, graph.LocalizationNodeScope, int,
		) (graph.BoundedNodeProjection, error) {
			return graph.BoundedNodeProjection{}, sentinel
		})
		_, err := readOverlayBaseNodes(context.Background(), reader, "file.go", &total)
		require.ErrorIs(t, err, sentinel)
	})

	t.Run("invalid_projection", func(t *testing.T) {
		total := 0
		reader := overlayLayerFileReaderFunc(func(
			context.Context, string, graph.LocalizationNodeScope, int,
		) (graph.BoundedNodeProjection, error) {
			return graph.BoundedNodeProjection{Total: 1}, nil
		})
		_, err := readOverlayBaseNodes(context.Background(), reader, "file.go", &total)
		require.ErrorContains(t, err, "invalid bounded projection")
		require.Zero(t, total)
	})

	t.Run("nil_node", func(t *testing.T) {
		total := 0
		reader := overlayLayerFileReaderFunc(func(
			context.Context, string, graph.LocalizationNodeScope, int,
		) (graph.BoundedNodeProjection, error) {
			return graph.BoundedNodeProjection{Nodes: []*graph.Node{nil}, Total: 1}, nil
		})
		_, err := readOverlayBaseNodes(context.Background(), reader, "file.go", &total)
		require.ErrorContains(t, err, "invalid nil node")
		require.Zero(t, total)
	})

	t.Run("cancel_after_reader", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		total := 0
		reader := overlayLayerFileReaderFunc(func(
			context.Context, string, graph.LocalizationNodeScope, int,
		) (graph.BoundedNodeProjection, error) {
			cancel()
			return graph.BoundedNodeProjection{}, nil
		})
		_, err := readOverlayBaseNodes(ctx, reader, "file.go", &total)
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, total)
	})
}
