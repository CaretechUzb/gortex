package lsp

import (
	"strconv"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

type lspBatchCountingStore struct {
	graph.Store
	getNodeCalls          int
	getFileNodesCalls     int
	getOutEdgesCalls      int
	getInEdgesCalls       int
	getNodesBatchCalls    int
	getOutBatchCalls      int
	getInBatchCalls       int
	addBatchCalls         int
	reindexBatchCalls     int
	persistEdgeBatchCalls int
}

func (s *lspBatchCountingStore) GetNode(id string) *graph.Node {
	s.getNodeCalls++
	return s.Store.GetNode(id)
}

func (s *lspBatchCountingStore) GetFileNodes(path string) []*graph.Node {
	s.getFileNodesCalls++
	return s.Store.GetFileNodes(path)
}

func (s *lspBatchCountingStore) GetOutEdges(id string) []*graph.Edge {
	s.getOutEdgesCalls++
	return s.Store.GetOutEdges(id)
}

func (s *lspBatchCountingStore) GetInEdges(id string) []*graph.Edge {
	s.getInEdgesCalls++
	return s.Store.GetInEdges(id)
}

func (s *lspBatchCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesBatchCalls++
	return s.Store.GetNodesByIDs(ids)
}

func (s *lspBatchCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getOutBatchCalls++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *lspBatchCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getInBatchCalls++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *lspBatchCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatchCalls++
	s.Store.AddBatch(nodes, edges)
}

func (s *lspBatchCountingStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexBatchCalls++
	s.Store.ReindexEdges(batch)
}

func (s *lspBatchCountingStore) PersistEdgeAttributesBatch(_ []*graph.Edge) {
	s.persistEdgeBatchCalls++
}

func TestAddOverrideEdgesUsesConstantBatchStoreCalls(t *testing.T) {
	base := graph.New()
	child := &graph.Node{ID: "child", Name: "Child", Kind: graph.KindType, FilePath: "types.go", StartLine: 1, EndLine: 200}
	parent := &graph.Node{ID: "parent", Name: "Parent", Kind: graph.KindInterface, FilePath: "types.go", StartLine: 201, EndLine: 400}
	nodes := []*graph.Node{child, parent}
	edges := make([]*graph.Edge, 0, 200)
	for i := 0; i < 100; i++ {
		name := "Method" + strconv.Itoa(i)
		childMethod := &graph.Node{ID: "child-" + name, Name: name, Kind: graph.KindMethod, FilePath: "child.go", StartLine: i + 1, EndLine: i + 1}
		parentMethod := &graph.Node{ID: "parent-" + name, Name: name, Kind: graph.KindMethod, FilePath: "parent.go", StartLine: i + 1, EndLine: i + 1}
		nodes = append(nodes, childMethod, parentMethod)
		edges = append(edges,
			&graph.Edge{From: childMethod.ID, To: child.ID, Kind: graph.EdgeMemberOf},
			&graph.Edge{From: parentMethod.ID, To: parent.ID, Kind: graph.EdgeMemberOf},
		)
	}
	base.AddBatch(nodes, edges)
	counting := &lspBatchCountingStore{Store: base}
	result := &semantic.EnrichResult{}

	addOverrideEdges(counting, child, parent, "lsp-test", graph.OriginLSPDispatch, result)

	if result.EdgesAdded != 100 {
		t.Fatalf("expected 100 override edges, got %d", result.EdgesAdded)
	}
	if counting.getNodeCalls != 0 || counting.getFileNodesCalls != 0 || counting.getOutEdgesCalls != 0 || counting.getInEdgesCalls != 0 {
		t.Fatalf("point queries used: node=%d file=%d out=%d in=%d",
			counting.getNodeCalls, counting.getFileNodesCalls, counting.getOutEdgesCalls, counting.getInEdgesCalls)
	}
	if counting.getInBatchCalls != 1 || counting.getNodesBatchCalls != 1 || counting.getOutBatchCalls != 1 {
		t.Fatalf("batch reads must stay constant: in=%d nodes=%d out=%d",
			counting.getInBatchCalls, counting.getNodesBatchCalls, counting.getOutBatchCalls)
	}
	if counting.addBatchCalls != 1 {
		t.Fatalf("expected one AddBatch, got %d", counting.addBatchCalls)
	}
}

func TestLSPMutationBatchCollapsesWrites(t *testing.T) {
	base := graph.New()
	nodes := []*graph.Node{
		{ID: "a", Kind: graph.KindFunction, FilePath: "a.go", StartLine: 1, EndLine: 10},
		{ID: "b", Kind: graph.KindFunction, FilePath: "b.go", StartLine: 1, EndLine: 10},
		{ID: "c", Kind: graph.KindFunction, FilePath: "c.go", StartLine: 1, EndLine: 10},
	}
	promoteA := &graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2, Confidence: 0.7, Origin: graph.OriginASTInferred}
	promoteB := &graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3, Confidence: 0.7, Origin: graph.OriginASTInferred}
	rebind := &graph.Edge{From: "b", To: "a", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 4, Confidence: 0.7, Origin: graph.OriginASTInferred}
	base.AddBatch(nodes, []*graph.Edge{promoteA, promoteB, rebind})
	view := newLSPGraphView(nodes, []*graph.Edge{promoteA, promoteB, rebind})
	mutations := newLSPMutationBatch()
	semantic.ConfirmEdge(promoteA, "lsp-test")
	semantic.ConfirmEdge(promoteB, "lsp-test")
	mutations.stagePersist(promoteA)
	mutations.stagePersist(promoteB)
	oldTo := rebind.To
	rebind.To = "c"
	semantic.ConfirmEdge(rebind, "lsp-test")
	mutations.stageReindex(view, rebind, oldTo)
	mutations.stageAdd(view, semantic.NewSemanticEdge("c", "a", graph.EdgeCalls, "c.go", 5, "lsp-test"))
	counting := &lspBatchCountingStore{Store: base}

	mutations.apply(counting, nil)

	if counting.reindexBatchCalls != 1 || counting.addBatchCalls != 1 || counting.persistEdgeBatchCalls != 1 {
		t.Fatalf("writes must be set-oriented: reindex=%d add=%d persist=%d",
			counting.reindexBatchCalls, counting.addBatchCalls, counting.persistEdgeBatchCalls)
	}
}

func TestMatchCallableByFileLineUsesLiveGraphProjection(t *testing.T) {
	const filePath = "callable.go"
	function := &graph.Node{ID: "callable.go::Outer", Kind: graph.KindFunction, Name: "Outer", FilePath: filePath, StartLine: 10, EndLine: 20}
	parameter := &graph.Node{ID: function.ID + "#param:value", Kind: graph.KindParam, Name: "value", FilePath: filePath, StartLine: 10}
	closure := &graph.Node{ID: function.ID + "#closure@14", Kind: graph.KindClosure, Name: "closure@14", FilePath: filePath, StartLine: 14, EndLine: 16}
	method := &graph.Node{ID: "callable.go::Widget.Run", Kind: graph.KindMethod, Name: "Run", FilePath: filePath, StartLine: 30, EndLine: 35}
	variable := &graph.Node{ID: "callable.go::value", Kind: graph.KindVariable, Name: "value", FilePath: filePath, StartLine: 40, EndLine: 40}
	view := newLSPGraphView([]*graph.Node{parameter, function, closure, method, variable}, nil)

	for _, test := range []struct {
		name string
		line int
		want string
	}{
		{name: "function wins over same-line parameter", line: 10, want: function.ID},
		{name: "function body", line: 12, want: function.ID},
		{name: "inner closure", line: 15, want: closure.ID},
		{name: "method body", line: 32, want: method.ID},
		{name: "two-line tolerance", line: 28, want: method.ID},
		{name: "unrelated variable", line: 40, want: ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			matched := view.matchCallableByFileLine(filePath, test.line)
			if test.want == "" {
				if matched != nil {
					t.Fatalf("expected no callable, got %s", matched.ID)
				}
				return
			}
			if matched == nil || matched.ID != test.want {
				got := "<nil>"
				if matched != nil {
					got = matched.ID
				}
				t.Fatalf("expected %s, got %s", test.want, got)
			}
		})
	}
}
