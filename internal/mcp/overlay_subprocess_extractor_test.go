package mcp

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	parserlanguages "github.com/zzet/gortex/internal/parser/languages"
)

func newOverlaySubprocessExtractor(command string, args ...string) *parserlanguages.SubprocessExtractor {
	return parserlanguages.NewSubprocessExtractor(config.ExtractorPluginSpec{
		Language:   "overlay-plugin",
		Extensions: []string{".plug"},
		Command:    command,
		Args:       args,
	}, nil)
}

func writeSparsePluginOutput(t *testing.T, size int64, prefix string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plugin-output.json")
	file, err := os.Create(path)
	require.NoError(t, err)
	if prefix != "" {
		_, err = file.WriteString(prefix)
		require.NoError(t, err)
	}
	require.NoError(t, file.Truncate(size))
	require.NoError(t, file.Close())
	return path
}

func TestOverlaySubprocessExtractorFailuresDoNotBuildOrCacheReplacement(t *testing.T) {
	resultPrefix := `{"nodes":[{"kind":"function","name":"Synthesized"}]}`
	resultLimited := writeSparsePluginOutput(t, int64(overlayLayerParsedResultBytesMax), resultPrefix)
	stdoutOverflow := writeSparsePluginOutput(t, int64(overlayLayerParsedResultBytesMax)+1, "")

	tests := []struct {
		name          string
		extractor     *parserlanguages.SubprocessExtractor
		limitResource string
	}{
		{
			name:          "stdout limit",
			extractor:     newOverlaySubprocessExtractor("cat", stdoutOverflow),
			limitResource: "stdout_bytes",
		},
		{
			name:          "combined result limit",
			extractor:     newOverlaySubprocessExtractor("cat", resultLimited),
			limitResource: "result_bytes",
		},
		{
			name:      "malformed output",
			extractor: newOverlaySubprocessExtractor("sh", "-c", "cat >/dev/null; printf 'not-json'"),
		},
		{
			name:      "nonzero command",
			extractor: newOverlaySubprocessExtractor("sh", "-c", "cat >/dev/null; printf 'null'; exit 7"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, targetFile, _, store := setupOverlayExtractorServer(t, test.extractor)
			graphPath := filepath.Base(targetFile)
			old := &graph.Node{ID: graphPath + "::Old", Kind: graph.KindFunction, Name: "Old", FilePath: graphPath}
			store.AddNode(old)

			ctx := WithSessionID(context.Background(), "bounded-plugin-session")
			ctx = withOverlayRequestSnapshot(ctx, &overlayRequestSnapshot{
				sessionID: "bounded-plugin-session",
				files:     []daemon.OverlayFile{{Path: targetFile, Content: "overlay"}},
				canonical: true,
			})
			view, err := server.buildOverlayViewForCtx(ctx)
			require.Error(t, err)
			require.Nil(t, view)
			if test.limitResource != "" {
				require.ErrorIs(t, err, parser.ErrExtractionLimit)
				var limitErr *parser.ExtractionLimitError
				require.ErrorAs(t, err, &limitErr)
				require.Equal(t, test.limitResource, limitErr.Resource)
			} else {
				require.NotErrorIs(t, err, parser.ErrExtractionLimit)
			}
			_, cached := server.overlayLayerCache.Load("bounded-plugin-session")
			require.False(t, cached, "failed parsing must not cache a file-only replacement")
			baseNodes := store.Store.GetFileNodes(graphPath)
			require.Len(t, baseNodes, 1)
			require.Equal(t, old.ID, baseNodes[0].ID, "durable declaration must remain visible in the base graph")
			fileCalls, _ := store.snapshotFileCalls()
			require.Empty(t, fileCalls, "strict subprocess failures must stop before replacement identity reads")
		})
	}
}

func TestOverlaySubprocessLegacyExtractStillDegradesOperationalFailure(t *testing.T) {
	extractor := newOverlaySubprocessExtractor("sh", "-c", "cat >/dev/null; printf 'not-json'")
	result, err := extractor.Extract("target.plug", []byte("overlay"))
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Len(t, result.Nodes, 1)
	require.Equal(t, graph.KindFile, result.Nodes[0].Kind)
	require.Empty(t, result.Edges)
}

func TestConstructOverlayLayerTrustedLegacyExtractorPathIsUnchanged(t *testing.T) {
	server, targetFile, _, _, extractor := setupOverlayLayerBoundServer(t)
	extractor.extract = func(_ int, filePath string, _ []byte) (*parser.ExtractionResult, error) {
		return overlayRawExtractionResult(filePath, 1, 0), nil
	}
	layer, paths, err := server.constructOverlayLayer(context.Background(), []daemon.OverlayFile{{Path: targetFile, Content: "overlay"}})
	require.NoError(t, err)
	require.NotNil(t, layer)
	require.Len(t, paths, 1)
	require.Equal(t, 1, extractor.callCount())
}
