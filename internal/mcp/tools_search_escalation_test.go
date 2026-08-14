package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// floodTestServer builds a server whose candidate ordering for the
// token "extensions" is docCount KindDoc sections and nothing else:
// the junk sections own the token outright, so with the default
// corpus=code filter every candidate a bounded fetch reaches is
// dropped — the shape a suffix-convention query takes on a repo whose
// doc/junk files share the naming tokens. The one real code symbol
// (WidgetExtensions) is NOT in that ordering; it carries the token
// only as part of its full name, so it is reachable solely through
// the engine's substring fill, whose budget is the fetch depth. That
// is what makes the escalation depth observable — a fetch deep enough
// to leave slack past the doc flood is the only one that rescues it.
func floodTestServer(t *testing.T, docCount int) *Server {
	t.Helper()
	g := graph.New()
	ob := newOrderedBackend()

	id := "pkg/WidgetExtensions.go::WidgetExtensions"
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindType, Name: "WidgetExtensions",
		FilePath: "pkg/WidgetExtensions.go", StartLine: 1, EndLine: 5, Language: "go",
	})

	for i := 0; i < docCount; i++ {
		docID := fmt.Sprintf("junk/list%d.txt::sec%d", i, i)
		g.AddNode(&graph.Node{
			ID: docID, Kind: graph.KindDoc, Name: "Extensions",
			FilePath: fmt.Sprintf("junk/list%d.txt", i), StartLine: 1, EndLine: 3, Language: "text",
		})
		ob.put("extensions", docID)
	}
	// The spelled-out name is its own token and answers with the code
	// symbol alone — a query for it is never flooded.
	ob.put("widgetextensions", id)

	eng := query.NewEngine(g)
	eng.SetSearch(ob)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	srv.RunAnalysis()
	return srv
}

// floodTestServerN is floodTestServer with codeCount rescuable code
// symbols behind the doc flood — the multi-page rescue shape from the
// PR review: a partially-successful shallow rescue must not strand a
// later cursor page a deeper fetch would fill. Same ordering premise as
// floodTestServer — the doc sections are the whole "extensions"
// ordering and the code symbols ride the substring fill — so the depth
// a fetch reaches decides how many of them survive the corpus filter:
// docCount+5 of budget rescues 5, a deeper fetch rescues all.
func floodTestServerN(t *testing.T, docCount, codeCount int) *Server {
	t.Helper()
	g := graph.New()
	ob := newOrderedBackend()

	for i := 0; i < codeCount; i++ {
		id := fmt.Sprintf("pkg/w%d.go::WidgetExtensions%d", i, i)
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindType, Name: fmt.Sprintf("WidgetExtensions%d", i),
			FilePath: fmt.Sprintf("pkg/w%d.go", i), StartLine: 1, EndLine: 5, Language: "go",
		})
	}
	for i := 0; i < docCount; i++ {
		docID := fmt.Sprintf("junk/list%d.txt::sec%d", i, i)
		g.AddNode(&graph.Node{
			ID: docID, Kind: graph.KindDoc, Name: "Extensions",
			FilePath: fmt.Sprintf("junk/list%d.txt", i), StartLine: 1, EndLine: 3, Language: "text",
		})
		ob.put("extensions", docID)
	}

	eng := query.NewEngine(g)
	eng.SetSearch(ob)
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
// must be skipped, not paid a second time. The ordering is all-doc, so
// no depth can ever rescue a code symbol and the escalation loop runs
// to its own stopping rule.
func TestSearchSymbols_EscalationSkipsDuplicateDepth(t *testing.T) {
	g := graph.New()
	ob := newOrderedBackend()
	for i := 0; i < 2100; i++ {
		docID := fmt.Sprintf("junk/list%d.txt::sec%d", i, i)
		g.AddNode(&graph.Node{
			ID: docID, Kind: graph.KindDoc, Name: "Extensions",
			FilePath: fmt.Sprintf("junk/list%d.txt", i), StartLine: 1, EndLine: 3, Language: "text",
		})
		ob.put("extensions", docID)
	}
	eng := query.NewEngine(g)
	eng.SetSearch(ob)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	srv.RunAnalysis()

	resp := searchResp(t, srv, map[string]any{
		"query": "Extensions", "limit": 100, "cursor": encodeCursor(400),
	})
	require.Empty(t, respIDs(resp), "an all-doc corpus yields no code page")
	limits := ob.searchLimits()
	require.NotEmpty(t, limits, "the ordered backend must have intercepted the Search calls")
	deep := 0
	for _, l := range limits {
		if l >= 2000 {
			deep++
		}
	}
	require.LessOrEqualf(t, deep, 1,
		"capped escalation depths must not repeat an identical query; call limits: %v", limits)
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
