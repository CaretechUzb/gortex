package stdbench

import (
	"math"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

func TestEvaluate_KnownRetriever(t *testing.T) {
	ds := Dataset{
		Name: "synthetic",
		Queries: []Query{
			{ID: "q1", Text: "x", Relevant: map[string]int{"d1": 1}},
		},
	}
	// Retriever puts the one relevant doc at rank 2.
	retrieve := func(_ string, _ int) []string { return []string{"d2", "d1", "d3"} }

	m := Evaluate(ds, retrieve, []int{1, 5})
	require.Equal(t, 1, m.Scored)
	require.InDelta(t, 0.0, m.RecallAtK[1], 1e-9, "d1 is not in the top 1")
	require.InDelta(t, 1.0, m.RecallAtK[5], 1e-9, "d1 is in the top 5")
	require.InDelta(t, 0.5, m.MRR, 1e-9, "first hit is at rank 2")
	// NDCG@10: grade 1 at rank 2 → 1/log2(3); ideal DCG → 1/log2(2)=1.
	require.InDelta(t, 1.0/math.Log2(3), m.NDCGAt10, 1e-9)
}

func TestEvaluate_SkipsUnjudgedQueries(t *testing.T) {
	ds := Dataset{
		Queries: []Query{
			{ID: "q1", Text: "x", Relevant: map[string]int{"d1": 1}},
			{ID: "q2", Text: "y"}, // no relevance judgement
		},
	}
	m := Evaluate(ds, func(string, int) []string { return []string{"d1"} }, nil)
	require.Equal(t, 2, m.Queries)
	require.Equal(t, 1, m.Scored, "the unjudged query is excluded from the averages")
	require.InDelta(t, 1.0, m.RecallAtK[1], 1e-9)
}

func TestEvaluate_PerfectRanking(t *testing.T) {
	ds := Dataset{
		Queries: []Query{
			{ID: "q1", Text: "x", Relevant: map[string]int{"d1": 3}},
		},
	}
	m := Evaluate(ds, func(string, int) []string { return []string{"d1", "d2"} }, []int{1})
	require.InDelta(t, 1.0, m.RecallAtK[1], 1e-9)
	require.InDelta(t, 1.0, m.MRR, 1e-9)
	require.InDelta(t, 1.0, m.NDCGAt10, 1e-9)
}

// TestEvaluate_EndToEndFTS runs the full evaluation path: load a JSONL
// benchmark, index its corpus into a real SQLite store's native symbol
// FTS, and score retrieval — the same wiring `gortex eval stdbench`
// uses, which is the same text path search_symbols serves.
func TestEvaluate_EndToEndFTS(t *testing.T) {
	ds, err := LoadContextBench("testdata/contextbench.jsonl")
	require.NoError(t, err)

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "stdbench.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	items := make([]graph.SymbolFTSItem, 0, len(ds.Corpus))
	for _, d := range ds.Corpus {
		// Same splitter the indexer's write side uses, so the read side's
		// query tokenisation lands on the same terms.
		items = append(items, graph.SymbolFTSItem{
			NodeID: d.ID,
			Tokens: strings.Join(search.Tokenize(d.Text), " "),
		})
	}
	require.NoError(t, store.BulkUpsertSymbolFTS("", items))
	require.NoError(t, store.BuildSymbolIndex())

	indexed, err := store.SymbolFTSCount()
	require.NoError(t, err)
	require.Equal(t, len(ds.Corpus), indexed,
		"every corpus document must reach the FTS — a partial corpus scores a benchmark nobody ran")

	retrieve := func(query string, k int) []string {
		hits, err := store.SearchSymbols(query, k)
		require.NoError(t, err)
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.NodeID)
		}
		return ids
	}

	m := Evaluate(ds, retrieve, nil)
	require.Equal(t, 2, m.Scored)
	require.Greater(t, m.RecallAtK[5], 0.0, "the FTS should surface the gold candidate")
	require.Greater(t, m.MRR, 0.0)
	// Both queries are multi-word, so they miss the store's exact-name
	// Tier 0 short-circuit and are ranked by FTS bm25 — which puts each
	// gold candidate first. Pinning the values keeps the assertion honest:
	// a ranking inversion, or a corpus that reached the index untokenised,
	// moves them.
	require.InDelta(t, 1.0, m.RecallAtK[5], 1e-9)
	require.InDelta(t, 1.0, m.MRR, 1e-9)
}
