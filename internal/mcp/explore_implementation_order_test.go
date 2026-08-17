package mcp

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestExpandImplementationTargetsCanonicalizesBackendOrder(t *testing.T) {
	owner := &graph.Node{ID: "order-owner", Kind: graph.KindInterface, Name: "Runner", FilePath: "repo/api.go"}
	implementations := []*graph.Node{
		{ID: "order-a", Kind: graph.KindType, Name: "A", FilePath: "repo/impl.go"},
		{ID: "order-b", Kind: graph.KindType, Name: "B", FilePath: "repo/impl.go"},
		{ID: "order-c", Kind: graph.KindType, Name: "C", FilePath: "repo/impl.go"},
		{ID: "order-d", Kind: graph.KindType, Name: "D", FilePath: "repo/impl.go"},
		{ID: "order-e", Kind: graph.KindType, Name: "E", FilePath: "repo/impl.go"},
	}
	base := graph.New()
	nodes := []*graph.Node{owner}
	nodes = append(nodes, implementations...)
	base.AddBatch(nodes, nil)
	identity := func(index int) graph.EdgeIdentity {
		kind := graph.EdgeImplements
		if index%2 == 0 {
			kind = graph.EdgeExtends
		}
		return graph.EdgeIdentity{From: implementations[index].ID, To: owner.ID, Kind: kind}
	}
	orders := [][]graph.EdgeIdentity{
		{identity(4), identity(3), identity(2), identity(1), identity(0)},
		{identity(2), identity(0), identity(4), identity(1), identity(3)},
	}
	want := []string{"order-a", "order-b", "order-c", "order-d"}
	for orderIndex, backendOrder := range orders {
		backendOrder := append([]graph.EdgeIdentity(nil), backendOrder...)
		original := append([]graph.EdgeIdentity(nil), backendOrder...)
		reader := &exploreImplementationFaultReader{Reader: base}
		reader.incoming = func(context.Context, []string, []graph.EdgeKind, int) (graph.BoundedEdgeIdentityProjection, error) {
			return graph.BoundedEdgeIdentityProjection{
				ByEndpoint: map[string][]graph.EdgeIdentity{owner.ID: backendOrder},
			}, nil
		}
		expanded := exploreImplementationTestServer(reader).expandImplementationTargets(
			context.Background(), []exploreTarget{{node: owner, score: 1}}, graph.LocalizationNodeScope{},
		)
		if len(expanded) != 1+len(want) {
			t.Fatalf("order %d expanded=%d, want %d", orderIndex, len(expanded), 1+len(want))
		}
		for index, id := range want {
			if expanded[index+1].node == nil || expanded[index+1].node.ID != id {
				t.Fatalf("order %d candidate %d=%#v, want %s", orderIndex, index, expanded[index+1].node, id)
			}
		}
		for index := range original {
			if backendOrder[index] != original[index] {
				t.Fatalf("order %d reader slice mutated: %#v, want %#v", orderIndex, backendOrder, original)
			}
		}
	}
}
