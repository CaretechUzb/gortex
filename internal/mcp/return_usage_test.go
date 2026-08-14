package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	wire "github.com/gortexhq/gcx-go"
	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// returnUsageServer builds a server whose graph has a function `Fetch`
// called from four sites: one discarding the result, one assigning it,
// one returning it, and one the classifier left unstamped.
func returnUsageServer(t *testing.T) (*Server, string) {
	t.Helper()
	g := graph.New()
	fetch := &graph.Node{
		ID: "pkg/fetch.go::Fetch", Kind: graph.KindFunction, Name: "Fetch",
		FilePath: "pkg/fetch.go", StartLine: 3,
		Meta: map[string]any{"signature": "func() (int, error)"},
	}
	callers := []*graph.Node{
		{ID: "pkg/a.go::drop", Kind: graph.KindFunction, Name: "drop", FilePath: "pkg/a.go", StartLine: 2},
		{ID: "pkg/a.go::keep", Kind: graph.KindFunction, Name: "keep", FilePath: "pkg/a.go", StartLine: 8},
		{ID: "pkg/b.go::relay", Kind: graph.KindFunction, Name: "relay", FilePath: "pkg/b.go", StartLine: 4},
		{ID: "pkg/b.go::opaque", Kind: graph.KindFunction, Name: "opaque", FilePath: "pkg/b.go", StartLine: 12},
	}
	g.AddNode(fetch)
	for _, n := range callers {
		g.AddNode(n)
	}
	g.AddEdge(&graph.Edge{
		From: "pkg/a.go::drop", To: fetch.ID, Kind: graph.EdgeCalls,
		FilePath: "pkg/a.go", Line: 3,
		Meta: map[string]any{graph.MetaReturnUsage: graph.ReturnUsageDiscarded},
	})
	g.AddEdge(&graph.Edge{
		From: "pkg/a.go::keep", To: fetch.ID, Kind: graph.EdgeCalls,
		FilePath: "pkg/a.go", Line: 9,
		Meta: map[string]any{graph.MetaReturnUsage: graph.ReturnUsageAssigned},
	})
	g.AddEdge(&graph.Edge{
		From: "pkg/b.go::relay", To: fetch.ID, Kind: graph.EdgeCalls,
		FilePath: "pkg/b.go", Line: 5,
		Meta: map[string]any{graph.MetaReturnUsage: graph.ReturnUsageReturned},
	})
	g.AddEdge(&graph.Edge{
		From: "pkg/b.go::opaque", To: fetch.ID, Kind: graph.EdgeCalls,
		FilePath: "pkg/b.go", Line: 13,
	})

	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil), fetch.ID
}

func findUsagesEdges(t *testing.T, srv *Server, args map[string]any) []map[string]any {
	t.Helper()
	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = args
	res, err := srv.handleFindUsages(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	var resp struct {
		Edges []map[string]any `json:"edges"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	return resp.Edges
}

func TestFindUsages_ReturnUsageLabels(t *testing.T) {
	srv, id := returnUsageServer(t)
	edges := findUsagesEdges(t, srv, map[string]any{"id": id})
	require.Len(t, edges, 4)

	byFrom := map[string]string{}
	for _, e := range edges {
		usage, _ := e["return_usage"].(string)
		byFrom[e["from"].(string)] = usage
	}
	assert.Equal(t, graph.ReturnUsageDiscarded, byFrom["pkg/a.go::drop"])
	assert.Equal(t, graph.ReturnUsageAssigned, byFrom["pkg/a.go::keep"])
	assert.Equal(t, graph.ReturnUsageReturned, byFrom["pkg/b.go::relay"])
	assert.Empty(t, byFrom["pkg/b.go::opaque"], "unstamped edge carries no label")
}

func TestFindUsages_ReturnUsageFilter(t *testing.T) {
	srv, id := returnUsageServer(t)
	edges := findUsagesEdges(t, srv, map[string]any{"id": id, "return_usage": "discarded"})
	require.Len(t, edges, 1)
	assert.Equal(t, "pkg/a.go::drop", edges[0]["from"])
	assert.Equal(t, graph.ReturnUsageDiscarded, edges[0]["return_usage"])
}

func TestFindUsages_ReturnUsageGroupedByFile(t *testing.T) {
	srv, id := returnUsageServer(t)
	groups := findUsagesGroups(t, srv, map[string]any{"id": id})
	found := map[string]bool{}
	for _, g := range groups {
		for _, u := range g.(map[string]any)["uses"].([]any) {
			if usage, ok := u.(map[string]any)["return_usage"].(string); ok {
				found[usage] = true
			}
		}
	}
	assert.True(t, found[graph.ReturnUsageDiscarded])
	assert.True(t, found[graph.ReturnUsageAssigned])
	assert.True(t, found[graph.ReturnUsageReturned])
}

func TestFindUsages_ReturnUsageGCXColumn(t *testing.T) {
	srv, id := returnUsageServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = map[string]any{"id": id, "format": "gcx"}
	res, err := srv.handleFindUsages(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	payload := res.Content[0].(mcplib.TextContent).Text
	dec := wire.NewDecoder(strings.NewReader(payload))
	h, err := dec.Header()
	require.NoError(t, err)
	require.Contains(t, h.Fields, "return_usage")
	rows, err := dec.All()
	require.NoError(t, err)
	require.Len(t, rows, 4)
	usages := map[string]string{}
	for _, r := range rows {
		usages[r["from"]] = r["return_usage"]
	}
	assert.Equal(t, graph.ReturnUsageDiscarded, usages["pkg/a.go::drop"])
	assert.Equal(t, graph.ReturnUsageReturned, usages["pkg/b.go::relay"])
}

func TestVerifyChange_ReturnUsageDistribution(t *testing.T) {
	srv, id := returnUsageServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "verify_change"
	req.Params.Arguments = map[string]any{
		"changes": `[{"symbol_id":"` + id + `","new_signature":"func() (string, error)"}]`,
	}
	res, err := srv.handleVerifyChange(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	var resp struct {
		ReturnUsage []struct {
			SymbolID     string         `json:"symbol_id"`
			CallSites    int            `json:"call_sites"`
			Counts       map[string]int `json:"counts"`
			Unclassified int            `json:"unclassified"`
		} `json:"return_usage"`
	}
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	require.Len(t, resp.ReturnUsage, 1)
	ru := resp.ReturnUsage[0]
	assert.Equal(t, id, ru.SymbolID)
	assert.Equal(t, 4, ru.CallSites)
	assert.Equal(t, 1, ru.Counts[graph.ReturnUsageDiscarded])
	assert.Equal(t, 1, ru.Counts[graph.ReturnUsageAssigned])
	assert.Equal(t, 1, ru.Counts[graph.ReturnUsageReturned])
	assert.Equal(t, 1, ru.Unclassified)
}

func TestVerifyChange_ReturnUsageCompactLine(t *testing.T) {
	srv, id := returnUsageServer(t)
	req := mcplib.CallToolRequest{}
	req.Params.Name = "verify_change"
	req.Params.Arguments = map[string]any{
		"changes": `[{"symbol_id":"` + id + `","new_signature":"func() (string, error)"}]`,
		"compact": true,
	}
	res, err := srv.handleVerifyChange(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)

	text := res.Content[0].(mcplib.TextContent).Text
	assert.Contains(t, text,
		"return_usage "+id+" call_sites:4 assigned:1 discarded:1 returned:1 unclassified:1")
}

// TestFindUsages_ReturnUsageEndToEnd drives the full chain: the Go
// extractor classifies real call sites, the edges land in a graph (with
// the unresolved targets bound to the callee the way the resolver
// does), and find_usages surfaces each site's label.
func TestFindUsages_ReturnUsageEndToEnd(t *testing.T) {
	src := []byte(`package main

func helper() int {
	return 1
}

func drop() {
	helper()
}

func keep() {
	v := helper()
	_ = v
}

func relay() int {
	return helper()
}
`)
	result, err := languages.NewGoExtractor().Extract("main.go", src)
	require.NoError(t, err)
	defer result.Tree.Release()

	g := graph.New()
	for _, n := range result.Nodes {
		g.AddNode(n)
	}
	for _, e := range result.Edges {
		// Bind the extractor's unresolved target onto the local
		// definition — the same join the resolver performs.
		if e.To == "unresolved::helper" {
			e.To = "main.go::helper"
		}
		g.AddEdge(e)
	}

	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	srv := NewServer(eng, g, nil, nil, zap.NewNop(), nil)

	edges := findUsagesEdges(t, srv, map[string]any{"id": "main.go::helper"})
	require.NotEmpty(t, edges)
	byFrom := map[string]string{}
	for _, e := range edges {
		usage, _ := e["return_usage"].(string)
		byFrom[e["from"].(string)] = usage
	}
	assert.Equal(t, graph.ReturnUsageDiscarded, byFrom["main.go::drop"])
	assert.Equal(t, graph.ReturnUsageAssigned, byFrom["main.go::keep"])
	assert.Equal(t, graph.ReturnUsageReturned, byFrom["main.go::relay"])
}
