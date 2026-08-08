package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

// TestRunAnalysis_FeedsBundleFingerprintsToSQLiteBackend proves the
// server wiring: when the backend store implements
// graph.BundleFingerprintSink (the sqlite store does), RunAnalysis
// hands it the analysis pass's per-package fingerprints so its bundle
// cache can validate cached bundles. A subsequent search through the
// store then serves cached bundles for unchanged packages.
func TestRunAnalysis_FeedsBundleFingerprintsToSQLiteBackend(t *testing.T) {
	s, err := store_sqlite.Open(filepath.Join(t.TempDir(), "wiring.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	// The store must satisfy the sink interface the server type-asserts.
	_, ok := graph.Store(s).(graph.BundleFingerprintSink)
	require.True(t, ok, "sqlite store must implement graph.BundleFingerprintSink")

	// Seed a tiny package + FTS so a search returns a bundle.
	s.AddNode(&graph.Node{ID: "pkg/x.go::A", Kind: graph.KindFunction, Name: "AlphaWidget", FilePath: "pkg/x.go", Language: "go"})
	s.AddNode(&graph.Node{ID: "pkg/x.go::B", Kind: graph.KindFunction, Name: "BetaWidget", FilePath: "pkg/x.go", Language: "go"})
	s.AddEdge(&graph.Edge{From: "pkg/x.go::A", To: "pkg/x.go::B", Kind: graph.EdgeCalls, FilePath: "pkg/x.go"})
	require.NoError(t, s.BulkUpsertSymbolFTS("", []graph.SymbolFTSItem{
		{NodeID: "pkg/x.go::A", Tokens: "alpha widget"},
		{NodeID: "pkg/x.go::B", Tokens: "beta widget"},
	}))
	require.NoError(t, s.BuildSymbolIndex())

	eng := query.NewEngine(s)
	srv := NewServer(eng, s, nil, nil, zap.NewNop(), nil)
	// Registered after the store's Close cleanup so it runs first: the
	// analysis-generation prune RunAnalysis spawns must finish before the
	// store closes and the TempDir is removed, or its WAL commit recreates
	// files under the directory mid-RemoveAll.
	t.Cleanup(srv.DrainBackground)

	// RunAnalysis must reach backendStore() == the sqlite store and feed
	// it fingerprints. After it, a query populates the cache; a graph
	// mutation WITHOUT a re-analysis is then served stale from cache —
	// proof the fingerprints were installed and the cache is live.
	srv.RunAnalysis()

	first, err := s.SearchSymbolBundles("widget", 10)
	require.NoError(t, err)
	var aEdges int
	for _, b := range first {
		if b.Node != nil && b.Node.ID == "pkg/x.go::A" {
			aEdges = len(b.OutEdges)
		}
	}
	require.Equal(t, 1, aEdges, "A should have one out-edge on the first query")

	// Mutate without re-running analysis: a content-addressed cache fed
	// by RunAnalysis serves the stale (1-edge) bundle because the
	// package fingerprint is unchanged.
	s.AddNode(&graph.Node{ID: "pkg/x.go::C", Kind: graph.KindFunction, Name: "GammaWidget", FilePath: "pkg/x.go", Language: "go"})
	s.AddEdge(&graph.Edge{From: "pkg/x.go::A", To: "pkg/x.go::C", Kind: graph.EdgeCalls, FilePath: "pkg/x.go"})

	second, err := s.SearchSymbolBundles("widget", 10)
	require.NoError(t, err)
	for _, b := range second {
		if b.Node != nil && b.Node.ID == "pkg/x.go::A" {
			assert.Equal(t, 1, len(b.OutEdges),
				"cache fed by RunAnalysis should serve the stale bundle until re-analysis")
		}
	}

	// Re-running analysis recomputes the fingerprint (the package's
	// edges changed), invalidating the entry. The next query is fresh.
	srv.RunAnalysis()
	third, err := s.SearchSymbolBundles("widget", 10)
	require.NoError(t, err)
	for _, b := range third {
		if b.Node != nil && b.Node.ID == "pkg/x.go::A" {
			assert.Equal(t, 2, len(b.OutEdges),
				"after re-analysis the bundle must reflect the new edge")
		}
	}
}
