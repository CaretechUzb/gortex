package analysis

import (
	"iter"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type nilCommunityCrossingStore struct {
	graph.Store
	edgesByKindCalls int
}

func (s *nilCommunityCrossingStore) CommunityCrossingsByKind(
	[]graph.EdgeKind,
	map[string]string,
) map[string]int {
	return nil
}

func (s *nilCommunityCrossingStore) NodeFanCounts(
	ids []string,
	fanInKinds []graph.EdgeKind,
	fanOutKinds []graph.EdgeKind,
) []graph.NodeFanRow {
	return s.Store.(graph.NodeFanAggregator).NodeFanCounts(ids, fanInKinds, fanOutKinds)
}

func (s *nilCommunityCrossingStore) EdgeAdjacencyForKinds(
	edgeKinds []graph.EdgeKind,
	nodeKinds []graph.NodeKind,
) iter.Seq[[2]string] {
	return s.Store.(graph.EdgeAdjacencyForKinds).EdgeAdjacencyForKinds(edgeKinds, nodeKinds)
}

func (s *nilCommunityCrossingStore) EdgesByKind(graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.edgesByKindCalls++
	return func(func(*graph.Edge) bool) {}
}

func TestFindHotspotsDoesNotFallbackWhenCrossingCapabilityReturnsNil(t *testing.T) {
	base := graph.New()
	base.AddNode(&graph.Node{
		ID: "repo/main.go::Run", Kind: graph.KindFunction, Name: "Run",
		FilePath: "repo/main.go", RepoPrefix: "repo",
	})
	store := &nilCommunityCrossingStore{Store: base}

	FindHotspots(store, &CommunityResult{
		NodeToComm: map[string]string{"repo/main.go::Run": "community-1"},
	}, 101)

	if store.edgesByKindCalls != 0 {
		t.Fatalf("EdgesByKind fallback calls = %d, want 0 for an implemented aggregate capability", store.edgesByKindCalls)
	}
}
