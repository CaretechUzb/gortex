package store_sqlite

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func addBatchFixture(nodeCount, edgeCount int) ([]*graph.Node, []*graph.Edge) {
	nodes := make([]*graph.Node, nodeCount)
	for i := range nodes {
		nodes[i] = &graph.Node{
			ID: fmt.Sprintf("repo/f%d.go::S%d", i%17, i), Kind: graph.KindFunction,
			Name: fmt.Sprintf("S%d", i), FilePath: fmt.Sprintf("f%d.go", i%17),
			RepoPrefix: "repo", Language: "go", Meta: map[string]any{"position": i},
		}
	}
	edges := make([]*graph.Edge, edgeCount)
	for i := range edges {
		edges[i] = &graph.Edge{
			From: nodes[i%len(nodes)].ID, To: nodes[(i*7+1)%len(nodes)].ID,
			Kind: graph.EdgeCalls, FilePath: fmt.Sprintf("f%d.go", i%17), Line: i,
			Confidence: 0.75, Meta: map[string]any{"candidate_count": i % 5},
		}
	}
	return nodes, edges
}

func TestAddBatchUsesBoundedMultiRowStatements(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_JSONB_INGEST", "0")
	store := openReindexReceiptTestStore(t)
	nodes, edges := addBatchFixture(100, 200)
	stats, err := store.addBatchSetOriented(nodes, edges)
	require.NoError(t, err)
	require.Equal(t, 100, stats.nodeRowsChanged)
	require.Equal(t, 200, stats.edgeRowsInserted)

	nodeRows := batchRowsForVariableLimit(store.batchVariableLimit, nodeInsertParams, nodeInsertMaxChunkSize)
	edgeRows := batchRowsForVariableLimit(store.batchVariableLimit, edgeInsertParams, edgeInsertMaxChunkSize)
	expectedNodeStatements := (len(nodes) + nodeRows - 1) / nodeRows
	expectedEdgeStatements := (len(edges) + edgeRows - 1) / edgeRows
	require.LessOrEqual(t, nodeRows, nodeInsertMaxChunkSize)
	require.LessOrEqual(t, edgeRows, edgeInsertMaxChunkSize)
	require.LessOrEqual(t, nodeRows*nodeInsertParams, store.batchVariableLimit-sqliteBatchVariableHeadroom)
	require.LessOrEqual(t, edgeRows*edgeInsertParams, store.batchVariableLimit-sqliteBatchVariableHeadroom)
	require.Equal(t, expectedNodeStatements, stats.nodeStatements)
	require.Equal(t, expectedEdgeStatements, stats.edgeStatements)
	require.Less(t, stats.nodeStatements, len(nodes), "node inserts must remain set-oriented")
	require.Less(t, stats.edgeStatements, len(edges), "edge inserts must remain set-oriented")
	require.Equal(t, 100, store.NodeCount())
	require.Equal(t, 200, store.EdgeCount())

	warm, err := store.addBatchSetOriented(nodes, edges)
	require.NoError(t, err)
	require.Zero(t, warm.nodeRowsChanged)
	require.Zero(t, warm.edgeRowsInserted)
	require.Equal(t, expectedNodeStatements, warm.nodeStatements)
	require.Equal(t, expectedEdgeStatements, warm.edgeStatements)
}

func TestAddBatchJSONBReturningMatchesPlaceholderReceipt(t *testing.T) {
	nodes, edges := addBatchFixture(nodeInsertMaxChunkSize+1, edgeInsertMaxChunkSize+1)

	t.Setenv("GORTEX_SQLITE_JSONB_INGEST", "0")
	placeholder := openMutationReceiptStore(t)
	placeholderToken := placeholder.BeginMutationReceipt()
	placeholderStats, err := placeholder.addBatchSetOriented(nodes, edges)
	require.NoError(t, err)
	placeholderReceipt := placeholder.EndMutationReceipt(placeholderToken)
	require.Greater(t, placeholderStats.nodeStatements, 1)
	require.Greater(t, placeholderStats.edgeStatements, 1)

	t.Setenv("GORTEX_SQLITE_JSONB_INGEST", "1")
	jsonbStore := openMutationReceiptStore(t)
	jsonbToken := jsonbStore.BeginMutationReceipt()
	jsonbStats, err := jsonbStore.addBatchSetOriented(nodes, edges)
	require.NoError(t, err)
	jsonbReceipt := jsonbStore.EndMutationReceipt(jsonbToken)

	require.Equal(t, placeholderStats.nodeRowsChanged, jsonbStats.nodeRowsChanged)
	require.Equal(t, placeholderStats.edgeRowsInserted, jsonbStats.edgeRowsInserted)
	require.Equal(t, 1, jsonbStats.nodeStatements)
	require.Equal(t, 1, jsonbStats.edgeStatements)
	require.Equal(t, placeholderReceipt, jsonbReceipt)
	require.True(t, jsonbReceipt.Complete)
	require.True(t, jsonbReceipt.ResolutionRelevant)
	require.Equal(t, placeholder.GetNode(nodes[3].ID), jsonbStore.GetNode(nodes[3].ID))
	require.Equal(t, placeholder.GetOutEdges(nodes[4].ID), jsonbStore.GetOutEdges(nodes[4].ID))

	warmToken := jsonbStore.BeginMutationReceipt()
	warmStats, err := jsonbStore.addBatchSetOriented(nodes, edges)
	require.NoError(t, err)
	warmReceipt := jsonbStore.EndMutationReceipt(warmToken)
	require.Zero(t, warmStats.nodeRowsChanged)
	require.Zero(t, warmStats.edgeRowsInserted)
	require.Equal(t, 1, warmStats.nodeStatements)
	require.Equal(t, 1, warmStats.edgeStatements)
	require.True(t, warmReceipt.Complete)
	require.False(t, warmReceipt.ResolutionRelevant)
	require.Empty(t, warmReceipt.ChangedFiles)
	require.Empty(t, warmReceipt.DefinitionFiles)
	require.Empty(t, warmReceipt.TargetNames)
	require.Empty(t, warmReceipt.TargetIDs)
	require.Empty(t, warmReceipt.ImportCandidates)
}

func TestAddBatchMatchesOrderedSingleRowSemantics(t *testing.T) {
	batched := openReindexReceiptTestStore(t)
	sequential := openReindexReceiptTestStore(t)
	nodes, edges := addBatchFixture(80, 170)
	// Same-ID last-write-wins node and duplicate logical edge exercise the two
	// conflict policies inside a multi-row statement.
	nodes = append(nodes,
		&graph.Node{ID: nodes[3].ID, Kind: graph.KindMethod, Name: "Updated", FilePath: "updated.go", RepoPrefix: "repo", Meta: map[string]any{"visibility": "public"}},
		&graph.Node{ID: graph.ProxyNodeID("remote", "remote/f.go::S"), Kind: graph.KindFunction, Origin: "remote:remote", Stub: true},
	)
	proxyID := nodes[len(nodes)-1].ID
	edges = append(edges, edges[4], &graph.Edge{From: proxyID, To: nodes[0].ID, Kind: graph.EdgeCalls})

	batched.AddBatch(nodes, edges)
	for _, node := range nodes {
		sequential.AddNode(node)
	}
	for _, edge := range edges {
		sequential.AddEdge(edge)
	}
	require.Equal(t, sequential.NodeCount(), batched.NodeCount())
	require.Equal(t, sequential.EdgeCount(), batched.EdgeCount())
	require.Equal(t, sequential.GetNode(nodes[3].ID), batched.GetNode(nodes[3].ID))
	require.Equal(t, sequential.GetOutEdges(nodes[4].ID), batched.GetOutEdges(nodes[4].ID))
	require.Nil(t, batched.GetNode(proxyID))
}

func TestAddBatchAnalysisPreflightIsBatched(t *testing.T) {
	store := openMutationReceiptStore(t)
	nodes, _ := addBatchFixture(901, 0)
	store.AddBatch(nodes, nil)
	buildMinimalAnalysisGeneration(t, store, "batched-node-preflight", 0, true)
	before := store.AnalysisMutationRevision()

	stats, err := store.addBatchSetOriented(nodes, nil)
	require.NoError(t, err)
	require.Equal(t, 3, stats.analysisNodeStatements, "901 identities should use three bounded SELECTs")
	require.Zero(t, stats.nodeRowsChanged)
	require.Equal(t, before, store.AnalysisMutationRevision())
	_, found, err := store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	require.True(t, found, "idempotent batch must preserve active analysis")
}

func TestAddBatchLaterChunkEncodingFailureRollsBackEarlierChunks(t *testing.T) {
	t.Setenv("GORTEX_SQLITE_JSONB_INGEST", "0")
	store := openReindexReceiptTestStore(t)
	nodes, _ := addBatchFixture(nodeInsertChunkSize+1, 0)
	nodes[len(nodes)-1].Meta = map[string]any{"unsupported": make(chan int)}
	require.Panics(t, func() { store.AddBatch(nodes, nil) })
	require.Zero(t, store.NodeCount(), "the first multi-row statement must roll back with the later failure")
}
