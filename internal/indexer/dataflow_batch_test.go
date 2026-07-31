package indexer

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type dataflowBatchCountingStore struct {
	graph.Store

	scanCalls, scanBatches                int
	paramBatchCalls, callBatchCalls       int
	nodeBatchCalls, outBatchCalls         int
	reindexCalls, reindexedEdges          int
	getNodeCalls, getInCalls, getOutCalls int
	addEdgeCalls, removeEdgeCalls         int
}

func newDataflowBatchCountingStore() *dataflowBatchCountingStore {
	return &dataflowBatchCountingStore{Store: graph.New()}
}

func (s *dataflowBatchCountingStore) ScanEdgesByKindsBatched(
	kinds []graph.EdgeKind,
	batchSize int,
	yield func([]*graph.Edge) bool,
) {
	s.scanCalls++
	var edges []*graph.Edge
	for _, kind := range kinds {
		for edge := range s.EdgesByKind(kind) {
			if edge != nil {
				edges = append(edges, edge)
			}
		}
	}
	for start := 0; start < len(edges); start += batchSize {
		end := start + batchSize
		if end > len(edges) {
			end = len(edges)
		}
		s.scanBatches++
		if !yield(edges[start:end]) {
			return
		}
	}
}

func (s *dataflowBatchCountingStore) GetDataflowParamEdgesByOwnerIDs(ids []string) map[string][]*graph.Edge {
	s.paramBatchCalls++
	all := s.GetInEdgesByNodeIDs(ids)
	out := make(map[string][]*graph.Edge, len(all))
	for owner, edges := range all {
		for _, edge := range edges {
			if edge != nil && edge.Kind == graph.EdgeParamOf {
				out[owner] = append(out[owner], edge)
			}
		}
	}
	return out
}

func (s *dataflowBatchCountingStore) GetDataflowCallEdgesByCallerIDs(ids []string) map[string][]*graph.Edge {
	s.callBatchCalls++
	all := s.Store.GetOutEdgesByNodeIDs(ids)
	out := make(map[string][]*graph.Edge, len(all))
	for caller, edges := range all {
		for _, edge := range edges {
			if edge != nil && edge.Kind == graph.EdgeCalls {
				out[caller] = append(out[caller], edge)
			}
		}
	}
	return out
}

func (s *dataflowBatchCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.nodeBatchCalls++
	return s.Store.GetNodesByIDs(ids)
}

func (s *dataflowBatchCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.outBatchCalls++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *dataflowBatchCountingStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexCalls++
	s.reindexedEdges += len(batch)
	s.Store.ReindexEdges(batch)
}

func (s *dataflowBatchCountingStore) GetNode(id string) *graph.Node {
	s.getNodeCalls++
	return s.Store.GetNode(id)
}

func (s *dataflowBatchCountingStore) GetInEdges(id string) []*graph.Edge {
	s.getInCalls++
	return s.Store.GetInEdges(id)
}

func (s *dataflowBatchCountingStore) GetOutEdges(id string) []*graph.Edge {
	s.getOutCalls++
	return s.Store.GetOutEdges(id)
}

func (s *dataflowBatchCountingStore) AddEdge(edge *graph.Edge) {
	s.addEdgeCalls++
	s.Store.AddEdge(edge)
}

func (s *dataflowBatchCountingStore) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	s.removeEdgeCalls++
	return s.Store.RemoveEdge(from, to, kind)
}

func TestMaterializeDataflowParamsUsesBatchesAndPreservesSemantics(t *testing.T) {
	store := newDataflowBatchCountingStore()
	const (
		caller  = "repo/caller.go::Caller"
		callee  = "repo/callee.go::Target"
		other   = "repo/callee.go::Other"
		param0  = "repo/callee.go::Target#param:0"
		param1  = "repo/callee.go::Target#param:1"
		argFrom = "repo/caller.go::value"
		result  = "repo/caller.go::result"
		file    = "repo/caller.go"
	)
	nodes := []*graph.Node{
		{ID: caller, Kind: graph.KindFunction, Name: "Caller", FilePath: file, RepoPrefix: "repo"},
		{ID: callee, Kind: graph.KindFunction, Name: "Target", FilePath: "repo/callee.go", RepoPrefix: "repo"},
		{ID: other, Kind: graph.KindFunction, Name: "Other", FilePath: "repo/callee.go", RepoPrefix: "repo"},
		{ID: param0, Kind: graph.KindParam, Name: "a", FilePath: "repo/callee.go", RepoPrefix: "repo", Meta: map[string]any{"position": 0}},
		{ID: param1, Kind: graph.KindParam, Name: "b", FilePath: "repo/callee.go", RepoPrefix: "repo", Meta: map[string]any{"position": float64(1)}},
	}
	argMeta := map[string]any{"arg_position": int64(1), "keep": "arg metadata"}
	returnMeta := map[string]any{
		"returns_to_call": true, "call_line": float64(17),
		"callee_target": "Target", "keep": "return metadata",
	}
	arg := &graph.Edge{
		From: argFrom, To: callee, Kind: graph.EdgeArgOf,
		FilePath: file, Line: 17, Origin: "static", Confidence: 0.73, Meta: argMeta,
	}
	ret := &graph.Edge{
		From: caller, To: result, Kind: graph.EdgeReturnsTo,
		FilePath: file, Line: 17, Origin: "dataflow", Confidence: 0.84, Meta: returnMeta,
	}
	store.AddBatch(nodes, []*graph.Edge{
		{From: param0, To: callee, Kind: graph.EdgeParamOf, FilePath: "repo/callee.go"},
		{From: param1, To: callee, Kind: graph.EdgeParamOf, FilePath: "repo/callee.go"},
		// Same-line fallback comes first; callee_target must select Target.
		{From: caller, To: other, Kind: graph.EdgeCalls, FilePath: file, Line: 17},
		{From: caller, To: callee, Kind: graph.EdgeCalls, FilePath: file, Line: 17},
		arg, ret,
	})

	idx := &Indexer{graph: store}
	idx.materializeDataflowParams()

	require.Equal(t, param1, arg.To)
	require.Equal(t, callee, ret.From)
	assert.Equal(t, argMeta, arg.Meta)
	assert.Equal(t, returnMeta, ret.Meta)
	assert.Equal(t, "static", arg.Origin)
	assert.Equal(t, 0.73, arg.Confidence)
	assert.Empty(t, edgesOfKind(store.Store.GetOutEdges(caller), graph.EdgeReturnsTo))
	calleeReturns := edgesOfKind(store.Store.GetOutEdges(callee), graph.EdgeReturnsTo)
	require.Len(t, calleeReturns, 1)
	assert.Equal(t, result, calleeReturns[0].To)

	assert.Equal(t, 1, store.scanCalls)
	assert.Equal(t, 1, store.scanBatches)
	assert.Equal(t, 1, store.paramBatchCalls)
	assert.Equal(t, 1, store.nodeBatchCalls)
	assert.Equal(t, 1, store.callBatchCalls)
	assert.Equal(t, 1, store.reindexCalls)
	assert.Equal(t, 2, store.reindexedEdges)
	assertDataflowScalarCallsZero(t, store)

	writes := store.reindexCalls
	revisions := store.EdgeIdentityRevisions()
	idx.materializeDataflowParams()
	assert.Equal(t, writes, store.reindexCalls, "warm replay must be write-free")
	assert.Equal(t, revisions, store.EdgeIdentityRevisions())
	assertDataflowScalarCallsZero(t, store)
}

func TestMaterializeDataflowParamsQueryAndWriteCountIsPerBatch(t *testing.T) {
	store := newDataflowBatchCountingStore()
	const (
		callee = "repo/callee.go::Target"
		param  = "repo/callee.go::Target#param:0"
	)
	store.AddBatch([]*graph.Node{
		{ID: callee, Kind: graph.KindFunction, FilePath: "repo/callee.go"},
		{ID: param, Kind: graph.KindParam, FilePath: "repo/callee.go", Meta: map[string]any{"position": 0}},
	}, []*graph.Edge{{From: param, To: callee, Kind: graph.EdgeParamOf}})

	count := dataflowRewriteBatchSize*2 + 7
	edges := make([]*graph.Edge, 0, count)
	for i := 0; i < count; i++ {
		edges = append(edges, &graph.Edge{
			From: fmt.Sprintf("repo/caller.go::v%d", i), To: callee,
			Kind: graph.EdgeArgOf, FilePath: "repo/caller.go", Line: i + 1,
			Meta: map[string]any{"arg_position": 0},
		})
	}
	store.AddBatch(nil, edges)

	(&Indexer{graph: store}).materializeDataflowParams()

	const batches = 3
	assert.Equal(t, batches, store.scanBatches)
	assert.Equal(t, batches, store.paramBatchCalls)
	assert.Equal(t, batches, store.nodeBatchCalls)
	assert.Equal(t, batches, store.reindexCalls)
	assert.Equal(t, count, store.reindexedEdges)
	assert.Zero(t, store.callBatchCalls)
	assertDataflowScalarCallsZero(t, store)
	for _, edge := range edges {
		assert.Equal(t, param, edge.To)
	}

	writes := store.reindexCalls
	(&Indexer{graph: store}).materializeDataflowParams()
	assert.Equal(t, writes, store.reindexCalls, "materialized arg_of edges must be warm no-ops")
}

func TestMaterializeDataflowParamsForFileUsesOneFrontierReadAndBatchRewrite(t *testing.T) {
	store := newDataflowBatchCountingStore()
	const (
		file    = "repo/caller.go"
		callee  = "repo/callee.go::Target"
		param   = "repo/callee.go::Target#param:0"
		argFrom = "repo/caller.go::value"
	)
	store.AddBatch([]*graph.Node{
		{ID: argFrom, Kind: graph.KindVariable, FilePath: file},
		{ID: callee, Kind: graph.KindFunction, FilePath: "repo/callee.go"},
		{ID: param, Kind: graph.KindParam, FilePath: "repo/callee.go", Meta: map[string]any{"position": 0}},
	}, []*graph.Edge{
		{From: param, To: callee, Kind: graph.EdgeParamOf},
		{From: argFrom, To: callee, Kind: graph.EdgeArgOf, FilePath: file, Line: 5, Meta: map[string]any{"arg_position": 0}},
	})

	(&Indexer{graph: store}).materializeDataflowParamsForFile(file, []*graph.Edge{{
		From: argFrom, To: callee, Kind: graph.EdgeArgOf, FilePath: file,
	}})

	assert.Equal(t, 1, store.outBatchCalls)
	assert.Equal(t, 1, store.paramBatchCalls)
	assert.Equal(t, 1, store.nodeBatchCalls)
	assert.Equal(t, 1, store.reindexCalls)
	assertDataflowScalarCallsZero(t, store)
}

func TestMaterializeDataflowParamsSQLiteEndToEnd(t *testing.T) {
	idx, store := newSQLiteIndexer(t)
	const (
		caller = "repo/caller.go::Caller"
		callee = "repo/callee.go::Target"
		param  = "repo/callee.go::Target#param:0"
		value  = "repo/caller.go::value"
		result = "repo/caller.go::result"
		file   = "repo/caller.go"
	)
	store.AddBatch([]*graph.Node{
		{ID: caller, Kind: graph.KindFunction, FilePath: file},
		{ID: callee, Kind: graph.KindFunction, FilePath: "repo/callee.go"},
		{ID: param, Kind: graph.KindParam, FilePath: "repo/callee.go", Meta: map[string]any{"position": 0}},
	}, []*graph.Edge{
		{From: param, To: callee, Kind: graph.EdgeParamOf},
		{From: caller, To: callee, Kind: graph.EdgeCalls, FilePath: file, Line: 11},
		{From: value, To: callee, Kind: graph.EdgeArgOf, FilePath: file, Line: 11, Meta: map[string]any{"arg_position": 0, "keep": "arg"}},
		{From: caller, To: result, Kind: graph.EdgeReturnsTo, FilePath: file, Line: 11, Meta: map[string]any{
			"returns_to_call": true, "call_line": 11, "callee_target": "Target", "keep": "return",
		}},
	})

	idx.materializeDataflowParams()

	args := edgesOfKind(store.GetOutEdges(value), graph.EdgeArgOf)
	require.Len(t, args, 1)
	assert.Equal(t, param, args[0].To)
	assert.Equal(t, "arg", args[0].Meta["keep"])
	returns := edgesOfKind(store.GetOutEdges(callee), graph.EdgeReturnsTo)
	require.Len(t, returns, 1)
	assert.Equal(t, result, returns[0].To)
	assert.Equal(t, "return", returns[0].Meta["keep"])

	before := store.EdgeIdentityRevisions()
	idx.materializeDataflowParams()
	assert.Equal(t, before, store.EdgeIdentityRevisions(), "SQLite warm replay must not mutate identities")
}

func edgesOfKind(edges []*graph.Edge, kind graph.EdgeKind) []*graph.Edge {
	var out []*graph.Edge
	for _, edge := range edges {
		if edge != nil && edge.Kind == kind {
			out = append(out, edge)
		}
	}
	return out
}

func assertDataflowScalarCallsZero(t *testing.T, store *dataflowBatchCountingStore) {
	t.Helper()
	assert.Zero(t, store.getNodeCalls, "dataflow must not issue scalar GetNode")
	assert.Zero(t, store.getInCalls, "dataflow must not issue scalar GetInEdges")
	assert.Zero(t, store.getOutCalls, "dataflow must not issue scalar GetOutEdges")
	assert.Zero(t, store.addEdgeCalls, "dataflow must not issue scalar AddEdge")
	assert.Zero(t, store.removeEdgeCalls, "dataflow must not issue scalar RemoveEdge")
}
