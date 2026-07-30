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
