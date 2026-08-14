package mcp

import (
	"context"
	"encoding/json"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search"
)

// usagesContextServer builds a server whose graph references a type `Foo`
// in three contexts: a parameter type, a return type, and a field type.
func usagesContextServer(t *testing.T) (*Server, string) {
	t.Helper()
	g := graph.New()
	foo := &graph.Node{ID: "pkg/foo.go::Foo", Kind: graph.KindType, Name: "Foo", FilePath: "pkg/foo.go"}
	param := &graph.Node{ID: "pkg/a.go::handle#p", Kind: graph.KindParam, Name: "in", FilePath: "pkg/a.go", StartLine: 2}
	fn := &graph.Node{ID: "pkg/a.go::make", Kind: graph.KindFunction, Name: "make", FilePath: "pkg/a.go", StartLine: 5}
	field := &graph.Node{ID: "pkg/b.go::Box.item", Kind: graph.KindField, Name: "item", FilePath: "pkg/b.go", StartLine: 3}
	for _, n := range []*graph.Node{foo, param, fn, field} {
		g.AddNode(n)
	}
	g.AddEdge(&graph.Edge{From: param.ID, To: foo.ID, Kind: graph.EdgeTypedAs, FilePath: "pkg/a.go", Line: 2})
	g.AddEdge(&graph.Edge{From: fn.ID, To: foo.ID, Kind: graph.EdgeReturns, FilePath: "pkg/a.go", Line: 5})
	g.AddEdge(&graph.Edge{From: field.ID, To: foo.ID, Kind: graph.EdgeTypedAs, FilePath: "pkg/b.go", Line: 3})

	eng := query.NewEngine(g)
	eng.SetSearch(search.NewNull())
	return NewServer(eng, g, nil, nil, zap.NewNop(), nil), foo.ID
}

func findUsagesGroups(t *testing.T, srv *Server, args map[string]any) []any {
	t.Helper()
	args["group_by"] = "file"
	req := mcplib.CallToolRequest{}
	req.Params.Name = "find_usages"
	req.Params.Arguments = args
	res, err := srv.handleFindUsages(context.Background(), req)
	require.NoError(t, err)
	require.False(t, res.IsError)
	var resp map[string]any
	require.NoError(t, json.Unmarshal([]byte(res.Content[0].(mcplib.TextContent).Text), &resp))
	groups, _ := resp["groups"].([]any)
	return groups
}

func collectContexts(groups []any) map[string]int {
	out := map[string]int{}
	for _, g := range groups {
		for _, u := range g.(map[string]any)["uses"].([]any) {
			if ctx, ok := u.(map[string]any)["context"].(string); ok {
				out[ctx]++
			}
		}
	}
	return out
}

func TestFindUsages_ContextLabels(t *testing.T) {
	srv, id := usagesContextServer(t)
	ctxs := collectContexts(findUsagesGroups(t, srv, map[string]any{"id": id}))
	require.Equal(t, 1, ctxs["parameter_type"], "param-typed usage")
	require.Equal(t, 1, ctxs["return_type"], "return-typed usage")
	require.Equal(t, 1, ctxs["field"], "field-typed usage")
}

func TestFindUsages_ContextFilter(t *testing.T) {
	srv, id := usagesContextServer(t)
	ctxs := collectContexts(findUsagesGroups(t, srv, map[string]any{"id": id, "context": "parameter_type"}))
	require.Equal(t, 1, ctxs["parameter_type"])
	require.Zero(t, ctxs["return_type"], "return_type must be filtered out")
	require.Zero(t, ctxs["field"], "field must be filtered out")
}
