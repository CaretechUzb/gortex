package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/semantic/lsp"
)

func TestOverlayLayerFailureIsNotCached(t *testing.T) {
	srv, targetFile, _, _, extractor := setupOverlayLayerBoundServer(t)
	extractor.extract = func(call int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
		if call == 0 {
			return overlayRawExtractionResult(filePath, overlayLayerParsedNodesMax+1, 0), nil
		}
		return overlayRawExtractionResult(filePath, 1, 0), nil
	}
	ctx := WithSessionID(context.Background(), "cache-session")
	ctx = withOverlayRequestSnapshot(ctx, &overlayRequestSnapshot{
		sessionID: "cache-session",
		files:     []daemon.OverlayFile{{Path: targetFile, Content: "package main"}},
		canonical: true,
	})

	view, err := srv.buildOverlayViewForCtx(ctx)
	require.Error(t, err)
	require.Nil(t, view)
	_, cached := srv.overlayLayerCache.Load("cache-session")
	require.False(t, cached)

	view, err = srv.buildOverlayViewForCtx(ctx)
	require.NoError(t, err)
	require.NotNil(t, view)
	_, cached = srv.overlayLayerCache.Load("cache-session")
	require.True(t, cached)
	require.Equal(t, 2, extractor.callCount())
}

func TestOverlayLayerFailureReturnsNoBranchOrSimulationPartial(t *testing.T) {
	t.Run("branch", func(t *testing.T) {
		srv, targetFile, _, _, extractor := setupOverlayLayerBoundServer(t)
		var result *parser.ExtractionResult
		extractor.extract = func(_ int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
			result = overlayRawExtractionResult(filePath, overlayLayerParsedNodesMax+1, 0)
			return result, nil
		}
		ids, paths, err := srv.runBranchQuery(
			context.Background(),
			[]daemon.OverlayFile{{Path: targetFile, Content: "package main"}},
			"get_callers",
			"target.go::Target",
			query.QueryOptions{},
		)
		require.Error(t, err)
		require.Nil(t, ids)
		require.Nil(t, paths)
		require.Nil(t, result.Tree)
		require.Nil(t, result.ConstValues)
	})

	t.Run("simulation", func(t *testing.T) {
		srv, targetFile, _, _, extractor := setupOverlayLayerBoundServer(t)
		var result *parser.ExtractionResult
		extractor.extract = func(_ int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
			result = overlayRawExtractionResult(filePath, 1, overlayLayerParsedEdgesMax+1)
			return result, nil
		}
		sim, err := srv.buildSimulation(
			context.Background(),
			[]lsp.WorkspaceEdit{fullFileSimulationEdit(targetFile, "package main\n")},
			false,
		)
		require.Error(t, err)
		require.Nil(t, sim)
		require.Nil(t, result.Tree)
		require.Nil(t, result.ConstValues)
	})
}

func TestConstructOverlayLayerResolverCancellationReturnsNoLayer(t *testing.T) {
	srv, targetFile, _, store, extractor := setupOverlayLayerBoundServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	var result *parser.ExtractionResult
	extractor.extract = func(_ int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
		result = &parser.ExtractionResult{
			Nodes: []*graph.Node{{ID: filePath + "::Caller", Name: "Caller", FilePath: filePath}},
			Edges: []*graph.Edge{{From: filePath + "::Caller", To: unresolvedPrefix + "call::Missing"}},
			Tree:  parser.NewParseTree(nil, []byte(filePath), "go"),
		}
		return result, nil
	}
	store.nameFn = func(
		context.Context, string, graph.LocalizationNodeScope, int,
	) (graph.BoundedNodeProjection, error) {
		cancel()
		return graph.BoundedNodeProjection{}, nil
	}

	layer, paths, err := srv.constructOverlayLayer(ctx, []daemon.OverlayFile{{Path: targetFile, Content: "package main"}})
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, layer)
	require.Nil(t, paths)
	require.Equal(t, "target.go::Caller", result.Nodes[0].ID)
	require.Equal(t, unresolvedPrefix+"call::Missing", result.Edges[0].To)
	require.Nil(t, result.Tree)
}

func TestConstructOverlayLayerAcceptsFullLiteralFileEnvelope(t *testing.T) {
	srv, targetFile, _, store, extractor := setupOverlayLayerBoundServer(t)
	extractor.extract = func(_ int, _ string, _ []byte) (*parser.ExtractionResult, error) {
		return &parser.ExtractionResult{}, nil
	}
	files := make([]daemon.OverlayFile, overlayRequestSnapshotMaxFiles)
	for index := range files {
		files[index] = daemon.OverlayFile{
			Path:    filepath.Join(filepath.Dir(targetFile), fmt.Sprintf("literal-%03d.go", index)),
			Content: "package main",
		}
	}
	layer, paths, err := srv.constructOverlayLayer(context.Background(), files)
	require.NoError(t, err)
	require.NotNil(t, layer)
	require.Len(t, paths, overlayRequestSnapshotMaxFiles)
	fileCalls, legacyFileReads := store.snapshotFileCalls()
	require.Len(t, fileCalls, overlayRequestSnapshotMaxFiles)
	require.Zero(t, legacyFileReads)
}
