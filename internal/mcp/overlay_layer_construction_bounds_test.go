package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/query"
)

func setupOverlayLayerBoundServer(t *testing.T) (*Server, string, string, *overlayLayerTestStore, *overlayLayerTestExtractor) {
	t.Helper()
	dir := t.TempDir()
	targetFile := filepath.Join(dir, "target.go")
	callerFile := filepath.Join(dir, "caller.go")
	require.NoError(t, os.WriteFile(targetFile, []byte("package main\n"), 0o644))
	require.NoError(t, os.WriteFile(callerFile, []byte("package main\n"), 0o644))

	base := graph.New()
	extractor := &overlayLayerTestExtractor{}
	registry := parser.NewRegistry()
	registry.Register(extractor)
	cfg := config.Default()
	idx := indexer.New(base, registry, cfg.Index, zap.NewNop())
	idx.SetRootPath(dir)
	idx.SetRepoPrefix("repo")

	store := &overlayLayerTestStore{Store: base}
	store.fileFn = func(
		context.Context, string, graph.LocalizationNodeScope, int,
	) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{}, nil
	}
	store.nameFn = func(
		context.Context, string, graph.LocalizationNodeScope, int,
	) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{}, nil
	}
	srv := &Server{
		graph:   store,
		indexer: idx,
		engine:  query.NewEngine(store),
		logger:  zap.NewNop(),
	}
	return srv, targetFile, callerFile, store, extractor
}

func overlayRawExtractionResult(filePath string, nodeCount, edgeCount int) *parser.ExtractionResult {
	nodes := make([]*graph.Node, nodeCount)
	if nodeCount > 0 {
		nodes[0] = &graph.Node{
			ID:       filePath + "::node",
			Name:     "node-" + filePath,
			FilePath: filePath,
			Meta:     map[string]any{"file": filePath},
		}
	}
	edges := make([]*graph.Edge, edgeCount)
	if edgeCount > 0 {
		edges[0] = &graph.Edge{
			From: filePath + "::node",
			To:   "external::target",
			Meta: map[string]any{"file": filePath},
		}
	}
	return &parser.ExtractionResult{
		Nodes:       nodes,
		Edges:       edges,
		Tree:        parser.NewParseTree(nil, []byte(filePath), "go"),
		ConstValues: []parser.ConstValue{{NodeID: filePath + "::const", FilePath: filePath, Value: "large-sidecar"}},
	}
}

func TestConstructOverlayLayerParsedEntryBounds(t *testing.T) {
	t.Run("exact_aggregate_with_nil_entries", func(t *testing.T) {
		srv, targetFile, callerFile, store, extractor := setupOverlayLayerBoundServer(t)
		var results []*parser.ExtractionResult
		extractor.extract = func(_ int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
			result := overlayRawExtractionResult(filePath, overlayLayerParsedNodesMax/2, overlayLayerParsedEdgesMax/2)
			results = append(results, result)
			return result, nil
		}
		layer, paths, err := srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{
			{Path: targetFile, Content: "package main"},
			{Path: callerFile, Content: "package main"},
		})
		require.NoError(t, err)
		require.NotNil(t, layer)
		require.Len(t, paths, 2)
		require.Len(t, results, 2)
		for _, result := range results {
			require.Nil(t, result.Tree)
			require.Nil(t, result.ConstValues)
		}
		fileCalls, legacyFileReads := store.snapshotFileCalls()
		require.Len(t, fileCalls, 2)
		require.Zero(t, legacyFileReads, "construction must not call GetFileNodes")
		view := graph.NewOverlaidView(store, layer)
		require.Len(t, view.GetFileNodes("target.go"), 1)
		require.Len(t, view.GetFileNodes("caller.go"), 1)
	})

	t.Run("nodes_plus_one_is_atomic", func(t *testing.T) {
		srv, targetFile, callerFile, _, extractor := setupOverlayLayerBoundServer(t)
		var first, overflow *parser.ExtractionResult
		extractor.extract = func(call int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
			count := overlayLayerParsedNodesMax / 2
			if call == 1 {
				count++
			}
			result := overlayRawExtractionResult(filePath, count, 1)
			if call == 0 {
				first = result
			} else {
				overflow = result
			}
			return result, nil
		}
		layer, paths, err := srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{
			{Path: targetFile, Content: "package main"},
			{Path: callerFile, Content: "package main"},
		})
		var limitErr *overlayLayerLimitError
		require.ErrorAs(t, err, &limitErr)
		require.Equal(t, "parsed nodes", limitErr.Resource)
		require.Nil(t, layer)
		require.Nil(t, paths)
		require.Equal(t, "target.go::node", first.Nodes[0].ID)
		require.Equal(t, "target.go::node", first.Edges[0].From)
		require.Equal(t, "target.go", first.Nodes[0].Meta["file"])
		require.Nil(t, first.Tree)
		require.Nil(t, first.ConstValues)
		require.NotNil(t, overflow)
		require.Nil(t, overflow.Tree)
		require.Nil(t, overflow.ConstValues)
	})

	t.Run("edges_plus_one_is_atomic", func(t *testing.T) {
		srv, targetFile, callerFile, _, extractor := setupOverlayLayerBoundServer(t)
		var first, overflow *parser.ExtractionResult
		extractor.extract = func(call int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
			count := overlayLayerParsedEdgesMax / 2
			if call == 1 {
				count++
			}
			result := overlayRawExtractionResult(filePath, 1, count)
			if call == 0 {
				first = result
			} else {
				overflow = result
			}
			return result, nil
		}
		layer, paths, err := srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{
			{Path: targetFile, Content: "package main"},
			{Path: callerFile, Content: "package main"},
		})
		var limitErr *overlayLayerLimitError
		require.ErrorAs(t, err, &limitErr)
		require.Equal(t, "parsed edges", limitErr.Resource)
		require.Nil(t, layer)
		require.Nil(t, paths)
		require.Equal(t, "target.go::node", first.Nodes[0].ID)
		require.Equal(t, "target.go::node", first.Edges[0].From)
		require.NotNil(t, overflow)
		require.Nil(t, overflow.Tree)
		require.Nil(t, overflow.ConstValues)
	})
}

func TestConstructOverlayLayerReleasesTreeOnEveryExtractorExit(t *testing.T) {
	t.Run("result_and_error", func(t *testing.T) {
		srv, targetFile, _, store, extractor := setupOverlayLayerBoundServer(t)
		sentinel := errors.New("extract failed")
		result := overlayRawExtractionResult("target.go", 1, 1)
		extractor.extract = func(int, string, []byte) (*parser.ExtractionResult, error) {
			return result, sentinel
		}
		layer, paths, err := srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{Path: targetFile, Content: "package main"}})
		require.ErrorIs(t, err, sentinel)
		require.Nil(t, layer)
		require.Nil(t, paths)
		require.Nil(t, result.Tree)
		require.Nil(t, result.ConstValues)
		fileCalls, _ := store.snapshotFileCalls()
		require.Empty(t, fileCalls)
	})

	t.Run("cancel_after_extract", func(t *testing.T) {
		srv, targetFile, _, store, extractor := setupOverlayLayerBoundServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		result := overlayRawExtractionResult("target.go", 1, 1)
		extractor.extract = func(int, string, []byte) (*parser.ExtractionResult, error) {
			cancel()
			return result, nil
		}
		layer, paths, err := srv.constructOverlayLayer(ctx, []daemon.OverlayFile{{Path: targetFile, Content: "package main"}})
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, layer)
		require.Nil(t, paths)
		require.Nil(t, result.Tree)
		require.Nil(t, result.ConstValues)
		fileCalls, _ := store.snapshotFileCalls()
		require.Empty(t, fileCalls)
	})

	t.Run("pre_canceled_skips_extract", func(t *testing.T) {
		srv, targetFile, _, _, extractor := setupOverlayLayerBoundServer(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		layer, paths, err := srv.constructOverlayLayer(ctx, []daemon.OverlayFile{{Path: targetFile, Content: "package main"}})
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, layer)
		require.Nil(t, paths)
		require.Zero(t, extractor.callCount())
	})

	t.Run("nil_result_fails_closed", func(t *testing.T) {
		srv, targetFile, _, _, extractor := setupOverlayLayerBoundServer(t)
		extractor.extract = func(int, string, []byte) (*parser.ExtractionResult, error) {
			return nil, nil
		}
		layer, paths, err := srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{Path: targetFile, Content: "package main"}})
		require.ErrorContains(t, err, "extractor returned nil result")
		require.Nil(t, layer)
		require.Nil(t, paths)
	})
}

func TestConstructOverlayLayerSnapshotsReusedExtractorPointers(t *testing.T) {
	srv, targetFile, callerFile, store, extractor := setupOverlayLayerBoundServer(t)
	sharedNode := &graph.Node{Meta: map[string]any{}}
	sharedEdge := &graph.Edge{Meta: map[string]any{}}
	shared := &parser.ExtractionResult{Nodes: []*graph.Node{sharedNode}, Edges: []*graph.Edge{sharedEdge}}
	extractor.extract = func(call int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
		sharedNode.ID = filePath + "::node"
		sharedNode.Name = fmt.Sprintf("name-%d", call)
		sharedNode.FilePath = filePath
		sharedNode.Meta["call"] = call
		sharedEdge.From = sharedNode.ID
		sharedEdge.To = "external::target"
		sharedEdge.Meta["call"] = call
		return shared, nil
	}

	layer, _, err := srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{
		{Path: targetFile, Content: "package main"},
		{Path: callerFile, Content: "package main"},
	})
	require.NoError(t, err)
	view := graph.NewOverlaidView(store, layer)
	targetNodes := view.GetFileNodes("target.go")
	callerNodes := view.GetFileNodes("caller.go")
	require.Len(t, targetNodes, 1)
	require.Len(t, callerNodes, 1)
	require.Equal(t, "name-0", targetNodes[0].Name)
	require.Equal(t, 0, targetNodes[0].Meta["call"])
	require.Equal(t, "name-1", callerNodes[0].Name)
	require.Equal(t, 1, callerNodes[0].Meta["call"])
	targetEdges := layer.OutEdgesByFromAll()["repo/target.go::node"]
	require.Len(t, targetEdges, 1)
	require.Equal(t, 0, targetEdges[0].Meta["call"])
}

func TestConstructOverlayLayerRequiresBoundedBaseReader(t *testing.T) {
	for _, deleted := range []bool{false, true} {
		name := "replacement"
		if deleted {
			name = "tombstone"
		}
		t.Run(name, func(t *testing.T) {
			srv, targetFile, _, store, extractor := setupOverlayLayerBoundServer(t)
			var parsed *parser.ExtractionResult
			extractor.extract = func(_ int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
				parsed = overlayRawExtractionResult(filePath, 1, 0)
				return parsed, nil
			}
			srv.graph = &overlayLayerLegacyStore{Store: store}
			layer, paths, err := srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{
				Path: targetFile, Content: "package main", Deleted: deleted,
			}})
			require.ErrorIs(t, err, graph.ErrBoundedLocalizationUnavailable)
			require.Nil(t, layer)
			require.Nil(t, paths)
			_, legacyFileReads := store.snapshotFileCalls()
			require.Zero(t, legacyFileReads)
			if !deleted {
				require.NotNil(t, parsed)
				require.Nil(t, parsed.Tree)
				require.Nil(t, parsed.ConstValues)
			}
		})
	}
}

func TestConstructOverlayLayerPreservesTombstoneAndReplacementIdentity(t *testing.T) {
	srv, targetFile, _, _, extractor := setupOverlayLayerBoundServer(t)
	base := graph.New()
	base.AddNode(&graph.Node{ID: "target.go::Old", Name: "Old", FilePath: "target.go"})
	store := &overlayLayerTestStore{Store: base}
	srv.graph = store
	extractor.extract = func(_ int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
		return &parser.ExtractionResult{Nodes: []*graph.Node{{ID: filePath + "::New", Name: "New", FilePath: filePath}}}, nil
	}

	layer, _, err := srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{Path: targetFile, Content: "package main"}})
	require.NoError(t, err)
	_, legacyFileReads := store.snapshotFileCalls()
	require.Zero(t, legacyFileReads)
	view := graph.NewOverlaidView(store, layer)
	require.Empty(t, view.FindNodesByName("Old"))
	require.Len(t, view.FindNodesByName("New"), 1)

	layer, _, err = srv.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{Path: targetFile, Deleted: true}})
	require.NoError(t, err)
	view = graph.NewOverlaidView(store, layer)
	require.True(t, layer.IsTombstone("target.go"))
	require.Empty(t, view.GetFileNodes("target.go"))
	require.Empty(t, view.FindNodesByName("Old"))
}
