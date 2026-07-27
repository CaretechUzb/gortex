package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/daemon"
	"github.com/zzet/gortex/internal/graph"
)

// probeGraph builds a graph holding `filler` non-matching symbols plus
// whatever the caller adds, standing in for a large multi-repo daemon graph.
func probeGraph(t *testing.T, filler int) *graph.Graph {
	t.Helper()
	g := graph.New()
	for i := range filler {
		g.AddNode(&graph.Node{
			ID:        fmt.Sprintf("filler/f%d.go::Unrelated%d", i, i),
			Name:      fmt.Sprintf("Unrelated%d", i),
			Kind:      graph.KindFunction,
			FilePath:  fmt.Sprintf("filler/f%d.go", i),
			StartLine: 1,
		})
	}
	return g
}

// TestSearchSymbols_FindsSymbolAmongManyNodes is the behavioural floor for the
// control-plane probe the Grep-redirect hook calls: a matching symbol must
// come back even when it is a needle in a large graph. This used to be served
// by an AllNodes() scan, which read-locked every shard at once and so queued
// behind the indexer's writers; the probe's 200ms budget expired first and the
// hook logged probed_miss / timed_out instead of a hit.
func TestSearchSymbols_FindsSymbolAmongManyNodes(t *testing.T) {
	g := probeGraph(t, 5000)
	g.AddNode(&graph.Node{
		ID:        "pkg/probe.go::ZebraProbeMarker",
		Name:      "ZebraProbeMarker",
		Kind:      graph.KindFunction,
		FilePath:  "pkg/probe.go",
		StartLine: 42,
	})

	c := &realController{graph: g}
	res, err := c.SearchSymbols(context.Background(), daemon.SearchSymbolsParams{
		Query: "ZebraProbeMarker",
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "ZebraProbeMarker", res.Hits[0].Name)
	assert.Equal(t, "pkg/probe.go", res.Hits[0].FilePath)
	assert.Equal(t, 42, res.Hits[0].Line)
	assert.Equal(t, string(graph.KindFunction), res.Hits[0].Kind)
}

// TestSearchSymbols_ExcludesFileAndImportNodes locks in the kind filter: the
// hook asks "is this pattern a real symbol", so a file path or import path
// that happens to contain the needle must not answer yes.
func TestSearchSymbols_ExcludesFileAndImportNodes(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "config/config.go", Name: "config/config.go",
		Kind: graph.KindFile, FilePath: "config/config.go",
	})
	g.AddNode(&graph.Node{
		ID: "import::example.com/config", Name: "example.com/config",
		Kind: graph.KindImport, FilePath: "main.go",
	})

	c := &realController{graph: g}
	res, err := c.SearchSymbols(context.Background(), daemon.SearchSymbolsParams{Query: "config"})
	require.NoError(t, err)
	assert.Empty(t, res.Hits, "file and import nodes must never satisfy a symbol probe")
}

// TestSearchSymbols_FileNodesDoNotCrowdOutSymbols is the regression for the
// bounded-fetch rewrite. Candidates now come from the name index, which cannot
// express the File/Import filter, so a query matching thousands of file paths
// could return a page the filter empties completely. The probe must widen the
// page rather than report a false "no such symbol".
func TestSearchSymbols_FileNodesDoNotCrowdOutSymbols(t *testing.T) {
	g := graph.New()
	// Far more matching file nodes than the first page holds.
	for i := range 1000 {
		p := fmt.Sprintf("pkg/config_%d.go", i)
		g.AddNode(&graph.Node{ID: p, Name: p, Kind: graph.KindFile, FilePath: p})
	}
	g.AddNode(&graph.Node{
		ID: "pkg/load.go::configLoader", Name: "configLoader",
		Kind: graph.KindFunction, FilePath: "pkg/load.go", StartLine: 7,
	})

	c := &realController{graph: g}
	res, err := c.SearchSymbols(context.Background(), daemon.SearchSymbolsParams{
		Query: "config",
		Limit: 5,
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "configLoader", res.Hits[0].Name)
}

// TestSearchSymbols_RespectsLimit keeps the probe's response bounded — the
// hook renders these inline, so an unbounded list would flood the agent.
func TestSearchSymbols_RespectsLimit(t *testing.T) {
	g := graph.New()
	for i := range 100 {
		name := fmt.Sprintf("handlerAlpha%d", i)
		g.AddNode(&graph.Node{
			ID: "h.go::" + name, Name: name,
			Kind: graph.KindFunction, FilePath: "h.go",
		})
	}

	c := &realController{graph: g}
	res, err := c.SearchSymbols(context.Background(), daemon.SearchSymbolsParams{
		Query: "handlerAlpha",
		Limit: 7,
	})
	require.NoError(t, err)
	assert.Len(t, res.Hits, 7)
}

// TestSearchSymbols_FiltersByRepo covers the repo-scoped probe on a
// multi-repo daemon, where one repo can be a thin slice of the graph.
func TestSearchSymbols_FiltersByRepo(t *testing.T) {
	g := graph.New()
	for i := range 800 {
		name := fmt.Sprintf("sharedName%d", i)
		g.AddNode(&graph.Node{
			ID: "other/x.go::" + name, Name: name, Kind: graph.KindFunction,
			FilePath: "other/x.go", RepoPrefix: "other",
		})
	}
	g.AddNode(&graph.Node{
		ID: "mine/y.go::sharedNameTarget", Name: "sharedNameTarget",
		Kind: graph.KindFunction, FilePath: "mine/y.go", RepoPrefix: "mine",
	})

	c := &realController{graph: g}
	res, err := c.SearchSymbols(context.Background(), daemon.SearchSymbolsParams{
		Query: "sharedName",
		Repo:  "mine",
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "sharedNameTarget", res.Hits[0].Name)
}

// TestSearchSymbols_PrefersExactName covers the fast path: an exact short-name
// match is answered from the per-shard byName bucket, so a large population of
// symbols that merely contain the query cannot crowd it out.
func TestSearchSymbols_PrefersExactName(t *testing.T) {
	g := graph.New()
	for i := range 5000 {
		name := fmt.Sprintf("configLoader%d", i)
		g.AddNode(&graph.Node{
			ID: "bulk.go::" + name, Name: name,
			Kind: graph.KindFunction, FilePath: "bulk.go",
		})
	}
	g.AddNode(&graph.Node{
		ID: "pkg/exact.go::config", Name: "config",
		Kind: graph.KindType, FilePath: "pkg/exact.go", StartLine: 3,
	})

	c := &realController{graph: g}
	res, err := c.SearchSymbols(context.Background(), daemon.SearchSymbolsParams{
		Query: "config",
		Limit: 10,
	})
	require.NoError(t, err)
	require.Len(t, res.Hits, 1)
	assert.Equal(t, "config", res.Hits[0].Name)
	assert.Equal(t, "pkg/exact.go", res.Hits[0].FilePath)
}

// TestSearchSymbols_EmptyQueryAndNilGraph covers the two short-circuits the
// hook relies on to distinguish "no signal" from "probed and missed".
func TestSearchSymbols_EmptyQueryAndNilGraph(t *testing.T) {
	c := &realController{graph: graph.New()}
	res, err := c.SearchSymbols(context.Background(), daemon.SearchSymbolsParams{Query: ""})
	require.NoError(t, err)
	assert.Empty(t, res.Hits)

	empty := &realController{}
	res, err = empty.SearchSymbols(context.Background(), daemon.SearchSymbolsParams{Query: "anything"})
	require.NoError(t, err)
	assert.Empty(t, res.Hits)
}
