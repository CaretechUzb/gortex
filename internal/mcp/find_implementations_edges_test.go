package mcp

import (
	"context"
	"strings"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// find_implementations' GCX payload promises a `.nodes` AND a `.edges`
// section (wire-format.md). The handler used to wrap the node list in
// an edge-less SubGraph, so the two halves disagreed: an implementation
// in `.nodes` with `edges count=0` right underneath. The edges carry
// the evidence (origin/confidence) — they must survive to the client.
func TestFindImplementations_GCXCarriesImplementsEdges(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/i.go::Storer", Kind: graph.KindInterface, Name: "Storer", FilePath: "pkg/i.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "pkg/s.go::DiskStore", Kind: graph.KindType, Name: "DiskStore", FilePath: "pkg/s.go", Language: "go"})
	g.AddEdge(&graph.Edge{
		From: "pkg/s.go::DiskStore", To: "pkg/i.go::Storer",
		Kind: graph.EdgeImplements, Confidence: 0.95, Origin: "ast_resolved",
	})

	idx := indexer.New(g, testRegistry(), config.Default().Index, zap.NewNop())
	srv := NewServer(query.NewEngine(g), g, idx, nil, zap.NewNop(), nil)

	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_implementations"
	req.Params.Arguments = map[string]any{"id": "pkg/i.go::Storer", "format": "gcx"}

	result, err := srv.handleFindImplementations(context.Background(), req)
	require.NoError(t, err)
	require.False(t, result.IsError)
	text := result.Content[0].(mcplib.TextContent).Text

	assert.Contains(t, text, "DiskStore", "implementation node must be present")
	assert.Contains(t, text, "implements", "edges section must carry the implements edge: %s", text)
	assert.False(t, strings.Contains(text, "count=0"),
		"edges section must not be empty when implementations exist: %s", text)

	// The enrich pass fills display fields on the payload copies — the
	// edge stored in the graph must stay untouched (Origin empty until a
	// resolver stamps it), or a read would mutate shared state.
	stored := g.GetInEdges("pkg/i.go::Storer")
	require.Len(t, stored, 1)
	assert.Empty(t, stored[0].Tier, "read path must not write display fields onto the stored edge")
}
