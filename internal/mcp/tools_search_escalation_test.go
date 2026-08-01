package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// floodTestServer builds a server whose BM25 head for the token
// "extensions" is fully occupied by doc-section nodes: one real code
// symbol (WidgetExtensions) plus docCount KindDoc nodes that all
// outrank it (single-token name, tripled term frequency). With the
// default corpus=code filter, every fetched candidate is dropped —
// the shape a suffix-convention query takes on a repo whose doc/junk
// files share the naming tokens.
func floodTestServer(t *testing.T, docCount int) *Server {
	t.Helper()
	g := graph.New()
	bm := search.NewBM25()

	id := "pkg/WidgetExtensions.go::WidgetExtensions"
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindType, Name: "WidgetExtensions",
		FilePath: "pkg/WidgetExtensions.go", StartLine: 1, EndLine: 5, Language: "go",
	})
	bm.Add(id, "WidgetExtensions", "pkg/WidgetExtensions.go", "")

	for i := 0; i < docCount; i++ {
		docID := fmt.Sprintf("junk/list%d.txt::sec%d", i, i)
		g.AddNode(&graph.Node{
			ID: docID, Kind: graph.KindDoc, Name: "Extensions",
			FilePath: fmt.Sprintf("junk/list%d.txt", i), StartLine: 1, EndLine: 3, Language: "text",
		})
		bm.Add(docID, "Extensions Extensions Extensions")
	}

	eng := query.NewEngine(g)
	eng.SetSearch(bm)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv
}

// floodTestServerN is floodTestServer with codeCount rescuable code
// symbols behind the doc flood — the multi-page rescue shape from the
// PR review: a partially-successful shallow rescue must not strand a
// later cursor page a deeper fetch would fill.
func floodTestServerN(t *testing.T, docCount, codeCount int) *Server {
	t.Helper()
	g := graph.New()
	bm := search.NewBM25()

	for i := 0; i < codeCount; i++ {
		id := fmt.Sprintf("pkg/w%d.go::WidgetExtensions%d", i, i)
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindType, Name: fmt.Sprintf("WidgetExtensions%d", i),
			FilePath: fmt.Sprintf("pkg/w%d.go", i), StartLine: 1, EndLine: 5, Language: "go",
		})
		bm.Add(id, fmt.Sprintf("WidgetExtensions%d", i), fmt.Sprintf("pkg/w%d.go", i), "")
	}
	for i := 0; i < docCount; i++ {
		docID := fmt.Sprintf("junk/list%d.txt::sec%d", i, i)
		g.AddNode(&graph.Node{
			ID: docID, Kind: graph.KindDoc, Name: "Extensions",
			FilePath: fmt.Sprintf("junk/list%d.txt", i), StartLine: 1, EndLine: 3, Language: "text",
		})
		bm.Add(docID, "Extensions Extensions Extensions")
	}

	eng := query.NewEngine(g)
	eng.SetSearch(bm)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv
}

// TestSearchSymbols_EscalationReachesCursorWindow: the review's
// confirmed pagination bug. 170 doc rows own the head; the ×5 refetch
// (identifier fast path: depth (20+10+5)×5 = 175) surfaces only the
// first 5 code symbols — a non-empty rescue — but the requested window
// starts at offset 20, so stopping there returns an empty page 2 with
// no cursor even though the ×25 fetch exposes all 30. Escalation must
// continue until offset+limit is reachable or the backend is exhausted.
func TestSearchSymbols_EscalationReachesCursorWindow(t *testing.T) {
	srv := floodTestServerN(t, 170, 30)
	resp := searchResp(t, srv, map[string]any{
		"query": "Extensions", "limit": 10, "cursor": encodeCursor(20),
	})
	ids := respIDs(resp)
	require.Lenf(t, ids, 10,
		"page 2 (offset 20, limit 10) must hold the remaining rescued symbols; got %v", ids)
	require.Equal(t, true, resp["fetch_escalated"], "the rescued response must carry fetch_escalated:true")
}

// TestSearchSymbols_EscalationSkipsDuplicateDepth: once the depth cap
// clamps a multiplier, the next multiplier collapses to the same
// effective depth — an identical re-query cannot change the outcome and
// must be skipped, not paid a second time.
func TestSearchSymbols_EscalationSkipsDuplicateDepth(t *testing.T) {
	g := graph.New()
	bm := search.NewBM25()
	for i := 0; i < 2100; i++ {
		docID := fmt.Sprintf("junk/list%d.txt::sec%d", i, i)
		g.AddNode(&graph.Node{
			ID: docID, Kind: graph.KindDoc, Name: "Extensions",
			FilePath: fmt.Sprintf("junk/list%d.txt", i), StartLine: 1, EndLine: 3, Language: "text",
		})
		bm.Add(docID, "Extensions Extensions Extensions")
	}
	counter := &countingBackend{BM25Backend: bm}
	eng := query.NewEngine(g)
	eng.SetSearch(counter)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	srv.RunAnalysis()

	resp := searchResp(t, srv, map[string]any{
		"query": "Extensions", "limit": 100, "cursor": encodeCursor(400),
	})
	require.Empty(t, respIDs(resp), "an all-doc corpus yields no code page")
	require.NotEmpty(t, counter.limits, "counting backend must intercept Search calls")
	deep := 0
	for _, l := range counter.limits {
		if l >= 2000 {
			deep++
		}
	}
	require.LessOrEqualf(t, deep, 1,
		"capped escalation depths must not repeat an identical query; deep-call limits: %v", counter.limits)
}

// countingBackend wraps the BM25 backend and records every Search
// limit, so a test can prove an identical capped depth is not re-paid.
type countingBackend struct {
	*search.BM25Backend
	limits []int
}

func (c *countingBackend) Search(q string, limit int) []search.SearchResult {
	c.limits = append(c.limits, limit)
	return c.BM25Backend.Search(q, limit)
}

// TestSearchSymbols_EscalatesWhenPostFilterEmptiesFetch is the core
// feature test: the bounded fetch returns only doc hits, the corpus
// filter empties the page, and the deeper refetch must rescue the
// real symbol sitting past the original fetch horizon.
func TestSearchSymbols_EscalatesWhenPostFilterEmptiesFetch(t *testing.T) {
	srv := floodTestServer(t, 60)
	resp := searchResp(t, srv, map[string]any{"query": "Extensions"})
	ids := respIDs(resp)
	require.Truef(t, ids["pkg/WidgetExtensions.go::WidgetExtensions"],
		"escalated refetch should rescue the code symbol past the flooded head; got %v", ids)
	require.Equal(t, true, resp["fetch_escalated"], "the rescued response must carry fetch_escalated:true")
}

// TestSearchSymbols_NoEscalationOnPrimaryHit: when the primary page
// already holds a surviving candidate, no escalation runs and the
// flag stays absent.
func TestSearchSymbols_NoEscalationOnPrimaryHit(t *testing.T) {
	srv := floodTestServer(t, 60)
	resp := searchResp(t, srv, map[string]any{"query": "WidgetExtensions"})
	require.NotEmpty(t, respIDs(resp))
	require.Nil(t, resp["fetch_escalated"], "fetch_escalated must be absent when the primary fetch survived")
}

// TestSearchSymbols_TrueMissDoesNotEscalate: a query with no raw
// candidates at all has nothing to rescue — the empty result returns
// without the escalation flag (and without paying the refetch).
func TestSearchSymbols_TrueMissDoesNotEscalate(t *testing.T) {
	srv := floodTestServer(t, 10)
	resp := searchResp(t, srv, map[string]any{"query": "zzqwxnomatch"})
	require.Empty(t, respIDs(resp))
	require.Nil(t, resp["fetch_escalated"], "a true miss must not set fetch_escalated")
}
