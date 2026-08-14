package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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

type overlayBoundedExtractorCall struct {
	path   string
	limits parser.ExtractionLimits
}

type overlayBoundedTestExtractor struct {
	mu          sync.Mutex
	calls       []overlayBoundedExtractorCall
	legacyCalls int
	bounded     func(int, context.Context, string, []byte, parser.ExtractionLimits) (*parser.ExtractionResult, parser.ExtractionUsage, error)
}

func (e *overlayBoundedTestExtractor) Language() string     { return "go" }
func (e *overlayBoundedTestExtractor) Extensions() []string { return []string{".go"} }

func (e *overlayBoundedTestExtractor) Extract(string, []byte) (*parser.ExtractionResult, error) {
	e.mu.Lock()
	e.legacyCalls++
	e.mu.Unlock()
	return nil, errors.New("legacy Extract must not be called for a bounded extractor")
}

func (e *overlayBoundedTestExtractor) ExtractBounded(
	ctx context.Context,
	filePath string,
	content []byte,
	limits parser.ExtractionLimits,
) (*parser.ExtractionResult, parser.ExtractionUsage, error) {
	e.mu.Lock()
	call := len(e.calls)
	e.calls = append(e.calls, overlayBoundedExtractorCall{path: filePath, limits: limits})
	fn := e.bounded
	e.mu.Unlock()
	if fn == nil {
		return &parser.ExtractionResult{}, parser.ExtractionUsage{RawNodes: 1}, nil
	}
	return fn(call, ctx, filePath, content, limits)
}

func (e *overlayBoundedTestExtractor) snapshotCalls() ([]overlayBoundedExtractorCall, int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]overlayBoundedExtractorCall(nil), e.calls...), e.legacyCalls
}

func setupOverlayExtractorServer(
	t *testing.T,
	ext parser.Extractor,
) (*Server, string, string, *overlayLayerTestStore) {
	t.Helper()
	dir := t.TempDir()
	extension := ".go"
	if extensions := ext.Extensions(); len(extensions) > 0 {
		extension = extensions[0]
	}
	targetFile := filepath.Join(dir, "target"+extension)
	callerFile := filepath.Join(dir, "caller"+extension)
	require.NoError(t, os.WriteFile(targetFile, []byte("overlay"), 0o644))
	require.NoError(t, os.WriteFile(callerFile, []byte("overlay"), 0o644))

	base := graph.New()
	registry := parser.NewRegistry()
	registry.Register(ext)
	cfg := config.Default()
	idx := indexer.New(base, registry, cfg.Index, zap.NewNop())
	idx.SetRootPath(dir)
	idx.SetRepoPrefix("repo")

	store := &overlayLayerTestStore{Store: base}
	store.fileFn = func(context.Context, string, graph.LocalizationNodeScope, int) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{}, nil
	}
	store.nameFn = func(context.Context, string, graph.LocalizationNodeScope, int) (graph.BoundedNodeProjection, error) {
		return graph.BoundedNodeProjection{}, nil
	}
	server := &Server{
		graph:   store,
		indexer: idx,
		engine:  query.NewEngine(store),
		logger:  zap.NewNop(),
	}
	return server, targetFile, callerFile, store
}

func TestConstructOverlayLayerBoundedExtractorSharesRequestBudgets(t *testing.T) {
	extractor := &overlayBoundedTestExtractor{}
	server, targetFile, callerFile, _ := setupOverlayExtractorServer(t, extractor)
	extractor.bounded = func(
		call int,
		_ context.Context,
		filePath string,
		_ []byte,
		limits parser.ExtractionLimits,
	) (*parser.ExtractionResult, parser.ExtractionUsage, error) {
		switch call {
		case 0:
			require.Equal(t, overlayLayerParsedNodesMax, limits.MaxNodes)
			require.Equal(t, overlayLayerParsedEdgesMax, limits.MaxEdges)
			require.Equal(t, overlayLayerParsedResultBytesMax, limits.MaxStdoutBytes)
			require.Equal(t, overlayLayerParsedResultBytesMax, limits.MaxResultBytes)
			return overlayRawExtractionResult(filePath, 1, 1), parser.ExtractionUsage{
				RawNodes:    overlayLayerParsedNodesMax - 1,
				RawEdges:    overlayLayerParsedEdgesMax,
				StdoutBytes: 10,
				ResultBytes: overlayLayerParsedResultBytesMax,
			}, nil
		case 1:
			require.Equal(t, 1, limits.MaxNodes)
			require.Zero(t, limits.MaxEdges, "zero remaining edges must be passed strictly")
			require.Zero(t, limits.MaxStdoutBytes, "zero remaining stdout must be passed strictly")
			require.Zero(t, limits.MaxResultBytes, "zero remaining result bytes must be passed strictly")
			return overlayRawExtractionResult(filePath, 1, 0), parser.ExtractionUsage{RawNodes: 1}, nil
		default:
			t.Fatalf("unexpected bounded extraction call %d", call)
			return nil, parser.ExtractionUsage{}, nil
		}
	}

	layer, paths, err := server.constructOverlayLayer(context.Background(), []daemon.OverlayFile{
		{Path: targetFile, Content: "overlay"},
		{Path: callerFile, Content: "overlay"},
	})
	require.NoError(t, err)
	require.NotNil(t, layer)
	require.Len(t, paths, 2)
	calls, legacyCalls := extractor.snapshotCalls()
	require.Len(t, calls, 2)
	require.Zero(t, legacyCalls)
}

func TestConstructOverlayLayerRejectsInvalidBoundedUsageWithoutPartialState(t *testing.T) {
	tests := []struct {
		name   string
		result func(string) *parser.ExtractionResult
		usage  parser.ExtractionUsage
		want   string
	}{
		{
			name: "missing mandatory file charge",
			result: func(string) *parser.ExtractionResult {
				return overlayRawExtractionResult("target.go", 0, 0)
			},
			usage: parser.ExtractionUsage{},
			want:  "negative or missing usage",
		},
		{
			name: "negative edge charge",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 1, 0)
			},
			usage: parser.ExtractionUsage{RawNodes: 1, RawEdges: -1},
			want:  "negative or missing usage",
		},
		{
			name: "stdout larger than result",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 1, 0)
			},
			usage: parser.ExtractionUsage{RawNodes: 1, StdoutBytes: 2, ResultBytes: 1},
			want:  "result usage is smaller",
		},
		{
			name: "returned nodes exceed raw charge",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 2, 0)
			},
			usage: parser.ExtractionUsage{RawNodes: 1},
			want:  "result exceeds reported raw usage",
		},
		{
			name: "raw charge exceeds passed remainder",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 1, 0)
			},
			usage: parser.ExtractionUsage{RawNodes: overlayLayerParsedNodesMax + 1},
			want:  "parsed nodes exceeds limit",
		},
		{
			name: "negative node charge",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 1, 0)
			},
			usage: parser.ExtractionUsage{RawNodes: -1},
			want:  "negative or missing usage",
		},
		{
			name: "negative stdout charge",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 1, 0)
			},
			usage: parser.ExtractionUsage{RawNodes: 1, StdoutBytes: -1},
			want:  "negative or missing usage",
		},
		{
			name: "negative result charge",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 1, 0)
			},
			usage: parser.ExtractionUsage{RawNodes: 1, ResultBytes: -1},
			want:  "negative or missing usage",
		},
		{
			name: "returned edges exceed raw charge",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 1, 1)
			},
			usage: parser.ExtractionUsage{RawNodes: 1},
			want:  "result exceeds reported raw usage",
		},
		{
			name: "raw edges exceed passed remainder",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 1, 0)
			},
			usage: parser.ExtractionUsage{RawNodes: 1, RawEdges: overlayLayerParsedEdgesMax + 1},
			want:  "parsed edges exceeds limit",
		},
		{
			name: "stdout exceeds passed remainder",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 1, 0)
			},
			usage: parser.ExtractionUsage{
				RawNodes: 1, StdoutBytes: overlayLayerParsedResultBytesMax + 1,
				ResultBytes: overlayLayerParsedResultBytesMax + 1,
			},
			want: "parsed stdout bytes exceeds limit",
		},
		{
			name: "result exceeds passed remainder",
			result: func(path string) *parser.ExtractionResult {
				return overlayRawExtractionResult(path, 1, 0)
			},
			usage: parser.ExtractionUsage{RawNodes: 1, ResultBytes: overlayLayerParsedResultBytesMax + 1},
			want:  "parsed result bytes exceeds limit",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			extractor := &overlayBoundedTestExtractor{}
			server, targetFile, _, store := setupOverlayExtractorServer(t, extractor)
			var returned *parser.ExtractionResult
			extractor.bounded = func(
				_ int, _ context.Context, filePath string, _ []byte, _ parser.ExtractionLimits,
			) (*parser.ExtractionResult, parser.ExtractionUsage, error) {
				returned = test.result(filePath)
				return returned, test.usage, nil
			}

			layer, paths, err := server.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{Path: targetFile, Content: "overlay"}})
			require.ErrorContains(t, err, test.want)
			require.Nil(t, layer)
			require.Nil(t, paths)
			require.NotNil(t, returned)
			require.Nil(t, returned.Tree)
			require.Nil(t, returned.ConstValues)
			fileCalls, _ := store.snapshotFileCalls()
			require.Empty(t, fileCalls)
		})
	}
}

func TestConstructOverlayLayerBoundedErrorAndCancellationCleanResult(t *testing.T) {
	t.Run("result and error", func(t *testing.T) {
		extractor := &overlayBoundedTestExtractor{}
		server, targetFile, _, store := setupOverlayExtractorServer(t, extractor)
		sentinel := errors.New("bounded extractor failed")
		result := overlayRawExtractionResult("target.go", 1, 1)
		extractor.bounded = func(int, context.Context, string, []byte, parser.ExtractionLimits) (*parser.ExtractionResult, parser.ExtractionUsage, error) {
			return result, parser.ExtractionUsage{}, sentinel
		}
		layer, paths, err := server.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{Path: targetFile, Content: "overlay"}})
		require.ErrorIs(t, err, sentinel)
		require.Nil(t, layer)
		require.Nil(t, paths)
		require.Nil(t, result.Tree)
		require.Nil(t, result.ConstValues)
		fileCalls, _ := store.snapshotFileCalls()
		require.Empty(t, fileCalls)
	})

	t.Run("parent cancellation wins", func(t *testing.T) {
		extractor := &overlayBoundedTestExtractor{}
		server, targetFile, _, store := setupOverlayExtractorServer(t, extractor)
		ctx, cancel := context.WithCancel(context.Background())
		result := overlayRawExtractionResult("target.go", 1, 1)
		extractor.bounded = func(int, context.Context, string, []byte, parser.ExtractionLimits) (*parser.ExtractionResult, parser.ExtractionUsage, error) {
			cancel()
			return result, parser.ExtractionUsage{}, errors.New("component failure")
		}
		layer, paths, err := server.constructOverlayLayer(ctx, []daemon.OverlayFile{{Path: targetFile, Content: "overlay"}})
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, layer)
		require.Nil(t, paths)
		require.Nil(t, result.Tree)
		require.Nil(t, result.ConstValues)
		fileCalls, _ := store.snapshotFileCalls()
		require.Empty(t, fileCalls)
	})

	t.Run("component deadline remains an ordinary live-parent error", func(t *testing.T) {
		extractor := &overlayBoundedTestExtractor{}
		server, targetFile, _, store := setupOverlayExtractorServer(t, extractor)
		ctx := context.Background()
		result := overlayRawExtractionResult("target.go", 1, 1)
		extractor.bounded = func(int, context.Context, string, []byte, parser.ExtractionLimits) (*parser.ExtractionResult, parser.ExtractionUsage, error) {
			return result, parser.ExtractionUsage{}, context.DeadlineExceeded
		}
		layer, paths, err := server.constructOverlayLayer(ctx, []daemon.OverlayFile{{Path: targetFile, Content: "overlay"}})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.NoError(t, ctx.Err(), "the parent request remains live")
		require.Nil(t, layer)
		require.Nil(t, paths)
		require.Nil(t, result.Tree)
		require.Nil(t, result.ConstValues)
		fileCalls, _ := store.snapshotFileCalls()
		require.Empty(t, fileCalls)
	})
}

func TestConstructOverlayLayerBoundedAggregatePlusOneFailsAtomically(t *testing.T) {
	tests := []struct {
		name         string
		first        parser.ExtractionUsage
		resource     string
		secondLimits func(*testing.T, parser.ExtractionLimits)
	}{
		{
			name:     "raw nodes",
			first:    parser.ExtractionUsage{RawNodes: overlayLayerParsedNodesMax},
			resource: "nodes",
			secondLimits: func(t *testing.T, limits parser.ExtractionLimits) {
				require.Zero(t, limits.MaxNodes)
			},
		},
		{
			name:     "raw edges",
			first:    parser.ExtractionUsage{RawNodes: 1, RawEdges: overlayLayerParsedEdgesMax},
			resource: "edges",
			secondLimits: func(t *testing.T, limits parser.ExtractionLimits) {
				require.Zero(t, limits.MaxEdges)
			},
		},
		{
			name:     "result bytes",
			first:    parser.ExtractionUsage{RawNodes: 1, StdoutBytes: 1, ResultBytes: overlayLayerParsedResultBytesMax},
			resource: "result_bytes",
			secondLimits: func(t *testing.T, limits parser.ExtractionLimits) {
				require.Zero(t, limits.MaxStdoutBytes)
				require.Zero(t, limits.MaxResultBytes)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			extractor := &overlayBoundedTestExtractor{}
			server, targetFile, callerFile, store := setupOverlayExtractorServer(t, extractor)
			var first, overflow *parser.ExtractionResult
			extractor.bounded = func(
				call int, _ context.Context, filePath string, _ []byte, limits parser.ExtractionLimits,
			) (*parser.ExtractionResult, parser.ExtractionUsage, error) {
				if call == 0 {
					first = overlayRawExtractionResult(filePath, 1, 0)
					return first, test.first, nil
				}
				test.secondLimits(t, limits)
				overflow = overlayRawExtractionResult(filePath, 1, 0)
				limit := int64(0)
				switch test.resource {
				case "nodes":
					limit = int64(limits.MaxNodes)
				case "edges":
					limit = int64(limits.MaxEdges)
				case "result_bytes":
					limit = int64(limits.MaxResultBytes)
				}
				return overflow, parser.ExtractionUsage{}, &parser.ExtractionLimitError{
					Resource: test.resource, Limit: limit, Observed: limit + 1,
				}
			}

			layer, paths, err := server.constructOverlayLayer(context.Background(), []daemon.OverlayFile{
				{Path: targetFile, Content: "overlay"},
				{Path: callerFile, Content: "overlay"},
			})
			require.ErrorIs(t, err, parser.ErrExtractionLimit)
			var limitErr *parser.ExtractionLimitError
			require.ErrorAs(t, err, &limitErr, "wrapping must preserve the typed parser limit")
			require.Equal(t, test.resource, limitErr.Resource)
			require.Nil(t, layer)
			require.Nil(t, paths)
			require.NotNil(t, first)
			require.NotNil(t, overflow)
			require.Nil(t, first.Tree)
			require.Nil(t, first.ConstValues)
			require.Nil(t, overflow.Tree)
			require.Nil(t, overflow.ConstValues)
			fileCalls, _ := store.snapshotFileCalls()
			require.Len(t, fileCalls, 1, "failed second extraction must not read or stage its base file")
		})
	}
}

func TestConstructOverlayLayerBoundedAcceptsFull257FileCohort(t *testing.T) {
	extractor := &overlayBoundedTestExtractor{}
	server, targetFile, _, store := setupOverlayExtractorServer(t, extractor)
	extractor.bounded = func(
		call int, _ context.Context, filePath string, _ []byte, limits parser.ExtractionLimits,
	) (*parser.ExtractionResult, parser.ExtractionUsage, error) {
		require.Equal(t, overlayLayerParsedNodesMax-call, limits.MaxNodes)
		require.Equal(t, overlayLayerParsedEdgesMax, limits.MaxEdges)
		require.Equal(t, overlayLayerParsedResultBytesMax-call, limits.MaxStdoutBytes)
		require.Equal(t, overlayLayerParsedResultBytesMax-call, limits.MaxResultBytes)
		return overlayRawExtractionResult(filePath, 1, 0), parser.ExtractionUsage{
			RawNodes: 1, ResultBytes: 1,
		}, nil
	}

	files := make([]daemon.OverlayFile, overlayRequestSnapshotMaxFiles)
	for index := range files {
		files[index] = daemon.OverlayFile{
			Path:    filepath.Join(filepath.Dir(targetFile), "bounded-"+fmt.Sprintf("%03d.go", index)),
			Content: "overlay",
		}
	}
	layer, paths, err := server.constructOverlayLayer(context.Background(), files)
	require.NoError(t, err)
	require.NotNil(t, layer)
	require.Len(t, paths, overlayRequestSnapshotMaxFiles)
	calls, legacyCalls := extractor.snapshotCalls()
	require.Len(t, calls, overlayRequestSnapshotMaxFiles)
	require.Zero(t, legacyCalls)
	fileCalls, _ := store.snapshotFileCalls()
	require.Len(t, fileCalls, overlayRequestSnapshotMaxFiles)
}
