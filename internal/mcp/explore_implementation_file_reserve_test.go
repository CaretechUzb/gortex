package mcp

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
)

func TestExpandImplementationTargetsReusesAdmittedNewFile(t *testing.T) {
	g := graph.New()
	owner := &graph.Node{ID: "reserve-owner", Kind: graph.KindInterface, Name: "Runner", FilePath: "repo/api.go"}
	implementations := []*graph.Node{
		{ID: "reserve-impl-a", Kind: graph.KindType, Name: "ImplA", FilePath: "repo/impl-a.go"},
		{ID: "reserve-impl-b", Kind: graph.KindType, Name: "ImplB", FilePath: "repo/impl-b.go"},
		{ID: "reserve-impl-c", Kind: graph.KindType, Name: "ImplC", FilePath: "repo/impl-c.go"},
		{ID: "reserve-impl-d", Kind: graph.KindType, Name: "ImplD", FilePath: "repo/impl-a.go"},
	}
	nodes := []*graph.Node{owner}
	edges := make([]*graph.Edge, 0, len(implementations))
	for _, implementation := range implementations {
		nodes = append(nodes, implementation)
		edges = append(edges, &graph.Edge{From: implementation.ID, To: owner.ID, Kind: graph.EdgeImplements})
	}
	g.AddBatch(nodes, edges)
	server := &Server{engine: query.NewEngine(g)}
	expanded := server.expandImplementationTargets(
		context.Background(), []exploreTarget{{node: owner, score: 1}}, graph.LocalizationNodeScope{},
	)
	if len(expanded) != 1+len(implementations) {
		t.Fatalf("expanded targets=%d, want %d", len(expanded), 1+len(implementations))
	}
	got := make(map[string]bool, len(expanded))
	for _, target := range expanded {
		if target.node != nil {
			got[target.node.ID] = true
		}
	}
	for _, implementation := range implementations {
		if !got[implementation.ID] {
			t.Fatalf("implementation sharing an admitted file was dropped: %s (%v)", implementation.ID, got)
		}
	}
}
