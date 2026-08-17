package indexer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func newExtractSourceTestIndexer(t *testing.T, cfg config.IndexConfig) *Indexer {
	t.Helper()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	idx := New(graph.New(), registry, cfg, zap.NewNop())
	t.Cleanup(idx.Close)
	return idx
}

func TestExtractSourcePreservesRawCoordinates(t *testing.T) {
	cfg := config.Default()
	idx := newExtractSourceTestIndexer(t, cfg.Index)
	source := append([]byte{0xef, 0xbb, 0xbf}, []byte("package main\n\nfunc Late() {}\n")...)

	result, err := idx.ExtractSource(t.Context(), "late.go", source)
	require.NoError(t, err)
	defer result.ReleaseTree()

	for _, node := range result.Nodes {
		if strings.HasSuffix(node.ID, "::Late") {
			require.Equal(t, 3, node.StartLine)
			return
		}
	}
	t.Fatal("Late declaration was not extracted")
}

func TestExtractSourceRefusesCommandTransforms(t *testing.T) {
	cfg := config.Default()
	cfg.Index.Transforms = []config.TransformRule{{
		Name:       "must-not-run",
		Extensions: []string{".go"},
		Command:    []string{"gortex-test-transform-must-not-run"},
	}}
	idx := newExtractSourceTestIndexer(t, cfg.Index)

	result, err := idx.ExtractSource(t.Context(), "late.go", []byte("package main\n\nfunc Late() {}\n"))
	require.Nil(t, result)
	require.ErrorContains(t, err, "does not guarantee coordinate preservation")
}

type unstablePreParserExtractor struct{}

func (unstablePreParserExtractor) Language() string     { return "unstable" }
func (unstablePreParserExtractor) Extensions() []string { return []string{".unstable"} }
func (unstablePreParserExtractor) Extract(string, []byte) (*parser.ExtractionResult, error) {
	return &parser.ExtractionResult{}, nil
}
func (unstablePreParserExtractor) PreParse(src []byte) []byte {
	return append(append([]byte(nil), src...), 'x')
}

func TestExtractSourceRefusesUnstablePreParser(t *testing.T) {
	registry := parser.NewRegistry()
	registry.Register(unstablePreParserExtractor{})
	cfg := config.Default()
	idx := New(graph.New(), registry, cfg.Index, zap.NewNop())
	t.Cleanup(idx.Close)

	result, err := idx.ExtractSource(t.Context(), "late.unstable", []byte("source\n"))
	require.Nil(t, result)
	require.ErrorContains(t, err, "pre-parse rewrite does not preserve source coordinates")
}

func TestExtractSourceCancellationReleasesRawAdmissionWaiter(t *testing.T) {
	cfg := config.Default()
	idx := newExtractSourceTestIndexer(t, cfg.Index)
	budget := newParseAdmissionBudget(1)
	idx.parseAdmission.Store(budget)

	held, err := acquireParseAdmission(t.Context(), 1, 0, nil, budget)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	errCh := make(chan error, 1)
	go func() {
		result, extractErr := idx.ExtractSource(ctx, "late.go", []byte("package main\n\nfunc Late() {}\n"))
		if result != nil {
			result.ReleaseTree()
		}
		errCh <- extractErr
	}()

	require.Eventually(t, func() bool {
		budget.mu.Lock()
		defer budget.mu.Unlock()
		return len(budget.waiters) == 1
	}, time.Second, time.Millisecond)
	cancel()

	select {
	case extractErr := <-errCh:
		require.ErrorIs(t, extractErr, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("ExtractSource did not observe admission cancellation")
	}
	held.Release()
	budget.mu.Lock()
	defer budget.mu.Unlock()
	require.Zero(t, budget.used)
	require.Empty(t, budget.waiters)
}
