package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestSQLiteReplaceDerivedContractsRollsBackEntireFrontier(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	oldMatch := &graph.Edge{From: "consumer", To: "provider", Kind: graph.EdgeMatches, FilePath: "consumer.go", Line: 10}
	oldBridge := &graph.Edge{From: "bridge::ws::project::contract", To: "contract", Kind: graph.EdgeBridges, FilePath: "contracts://bridges"}
	oldTopic := &graph.Edge{From: "provider", To: "topic::kafka::orders", Kind: graph.EdgeProducesTopic, FilePath: "provider.go", Line: 20}
	store.AddBatch([]*graph.Node{
		{ID: "consumer", Kind: graph.KindFunction, Name: "consumer", FilePath: "consumer.go"},
		{ID: "provider", Kind: graph.KindFunction, Name: "provider", FilePath: "provider.go"},
		{ID: "contract", Kind: graph.KindContract, Name: "contract", FilePath: "provider.go"},
		{ID: "bridge::ws::project::contract", Kind: graph.KindContractBridge, Name: "contract", FilePath: "contracts://bridges", WorkspaceID: "ws", ProjectID: "project"},
		{ID: "topic::kafka::orders", Kind: graph.KindTopic, Name: "orders", FilePath: "provider.go"},
	}, []*graph.Edge{oldMatch, oldBridge, oldTopic})
	_, err = store.writerDB.Exec(`CREATE TRIGGER fail_replacement_b
BEFORE INSERT ON nodes WHEN NEW.id = 'replacement-b'
BEGIN SELECT RAISE(ABORT, 'forced replacement failure'); END`)
	require.NoError(t, err)

	_, err = store.ReplaceDerivedContracts(graph.DerivedContractReplacement{
		RemoveEdges:         []*graph.Edge{oldMatch, oldTopic},
		RemoveBridgeNodeIDs: []string{"bridge::ws::project::contract"},
		Nodes: []*graph.Node{
			{ID: "replacement-a", Kind: graph.KindContractBridge, Name: "a", QualName: "contract.a", FilePath: "contracts://bridges"},
			{ID: "replacement-b", Kind: graph.KindContractBridge, Name: "b", QualName: "contract.b", FilePath: "contracts://bridges"},
		},
		TouchedTopicNodeIDs: []string{"topic::kafka::orders"},
	})
	require.Error(t, err)
	assert.NotNil(t, store.GetNode("bridge::ws::project::contract"))
	assert.NotNil(t, store.GetNode("topic::kafka::orders"))
	assert.Nil(t, store.GetNode("replacement-a"))
	assert.Nil(t, store.GetNode("replacement-b"))
	assertDerivedEdgePresent(t, store.GetOutEdges("consumer"), graph.EdgeMatches, "provider", 10)
	assertDerivedEdgePresent(t, store.GetOutEdges("bridge::ws::project::contract"), graph.EdgeBridges, "contract", 0)
	assertDerivedEdgePresent(t, store.GetOutEdges("provider"), graph.EdgeProducesTopic, "topic::kafka::orders", 20)
}

func assertDerivedEdgePresent(t *testing.T, edges []*graph.Edge, kind graph.EdgeKind, target string, line int) {
	t.Helper()
	for _, edge := range edges {
		if edge.Kind == kind && edge.To == target && edge.Line == line {
			return
		}
	}
	t.Fatalf("missing edge kind=%s target=%s line=%d in %#v", kind, target, line, edges)
}
