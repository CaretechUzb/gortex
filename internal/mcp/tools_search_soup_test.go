package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

// soupTestServer builds a single-repo server whose retrieval ordering
// is one token per symbol: each symbol answers to its own lowercased
// name and to nothing else. So a disjunct that the soup handling
// actually queries retrieves exactly its own node, and a symbol no
// disjunct names ("Unrelated") is unreachable — the merged result is a
// readout of which terms were queried, not of how they ranked.
func soupTestServer(t *testing.T, soupMode string) *Server {
	t.Helper()
	g := graph.New()
	names := []string{"AuthHandler", "LoginService", "SigninFlow", "CredentialStore", "Unrelated"}
	ob := newOrderedBackend()
	for _, n := range names {
		id := "pkg/" + n + ".go::" + n
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: n,
			FilePath: "pkg/" + n + ".go", StartLine: 1, EndLine: 5, Language: "go",
		})
		ob.put(n, id)
	}
	eng := query.NewEngine(g)
	eng.SetSearch(ob)
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)
	srv.SetSearchConfig(config.SearchConfig{KeywordSoupRewrite: soupMode})
	return srv
}

func runSoupSearch(t *testing.T, srv *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "search_symbols"
	req.Params.Arguments = args
	res, err := srv.handleSearchSymbols(context.Background(), req)
	require.NoError(t, err)
	require.Falsef(t, res.IsError, "search errored: %v", res.Content)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	return resp
}

// TestSearchSymbols_SoupSplitMerge confirms a degenerate OR-soup query
// in the default "split" mode (a) reports the keyword_soup class, (b)
// attaches a query_advice nudge with the split disjuncts, and (c) the
// OR-merge over those disjuncts surfaces every targeted symbol and
// nothing else -- a symbol none of the disjuncts names stays out.
func TestSearchSymbols_SoupSplitMerge(t *testing.T) {
	srv := soupTestServer(t, config.KeywordSoupSplit)
	resp := runSoupSearch(t, srv, map[string]any{
		"query": "AuthHandler OR LoginService OR SigninFlow OR CredentialStore",
	})

	require.Equal(t, "keyword_soup", resp["query_class"], "a soup query should report the keyword_soup class")

	advice, ok := resp["query_advice"].(map[string]any)
	require.True(t, ok, "split mode must attach query_advice")
	require.NotEmpty(t, advice["reason"])
	split, ok := advice["split_into"].([]any)
	require.True(t, ok, "query_advice should carry the split disjuncts")
	require.Len(t, split, 4)

	ids := map[string]bool{}
	for _, r := range resp["results"].([]any) {
		ids[r.(map[string]any)["id"].(string)] = true
	}
	for _, want := range []string{
		"pkg/AuthHandler.go::AuthHandler",
		"pkg/LoginService.go::LoginService",
		"pkg/SigninFlow.go::SigninFlow",
		"pkg/CredentialStore.go::CredentialStore",
	} {
		require.Truef(t, ids[want], "soup split-merge missed %s; got %v", want, ids)
	}
	require.Falsef(t, ids["pkg/Unrelated.go::Unrelated"],
		"no disjunct names Unrelated, so the merge must not surface it; got %v", ids)
}

// TestSearchSymbols_SoupOffMode confirms KeywordSoupRewrite:"off"
// disables the soup defense -- no advice and no split-merge. The
// class detector still reports the query's shape (keyword_soup),
// which only tunes rerank weights and is harmless; off mode is about
// suppressing the handling, not the classification.
func TestSearchSymbols_SoupOffMode(t *testing.T) {
	srv := soupTestServer(t, config.KeywordSoupOff)
	resp := runSoupSearch(t, srv, map[string]any{
		"query": "AuthHandler OR LoginService OR SigninFlow OR CredentialStore",
	})
	require.Nil(t, resp["query_advice"], "off mode must not attach query_advice")
}

// TestSearchSymbols_SoupNudgeMode confirms "nudge" mode attaches the
// advice but does NOT split -- query_advice has no split_into list.
func TestSearchSymbols_SoupNudgeMode(t *testing.T) {
	srv := soupTestServer(t, config.KeywordSoupNudge)
	resp := runSoupSearch(t, srv, map[string]any{
		"query": "AuthHandler OR LoginService OR SigninFlow OR CredentialStore",
	})
	advice, ok := resp["query_advice"].(map[string]any)
	require.True(t, ok, "nudge mode still attaches query_advice")
	require.NotEmpty(t, advice["reason"])
	require.Nil(t, advice["split_into"], "nudge mode must not split the soup")
}

// TestSearchSymbols_GenuineQueryNotSoup confirms a normal symbol query
// is untouched -- no class override, no advice.
func TestSearchSymbols_GenuineQueryNotSoup(t *testing.T) {
	srv := soupTestServer(t, config.KeywordSoupSplit)
	resp := runSoupSearch(t, srv, map[string]any{"query": "AuthHandler"})
	require.Nil(t, resp["query_advice"], "a genuine query must not get soup advice")
	require.NotEqual(t, "keyword_soup", resp["query_class"])
}

// TestSearchSymbols_SoupPinnedViaQueryClass confirms an agent can pin
// the keyword_soup class explicitly even when the detector would not
// have tripped (a single-operator query).
func TestSearchSymbols_SoupPinnedViaQueryClass(t *testing.T) {
	srv := soupTestServer(t, config.KeywordSoupSplit)
	resp := runSoupSearch(t, srv, map[string]any{
		"query":       "AuthHandler OR LoginService",
		"query_class": "keyword_soup",
	})
	require.Equal(t, "keyword_soup", resp["query_class"])
	advice, ok := resp["query_advice"].(map[string]any)
	require.True(t, ok, "pinned keyword_soup must engage the defense")
	require.NotEmpty(t, advice["reason"])
}
