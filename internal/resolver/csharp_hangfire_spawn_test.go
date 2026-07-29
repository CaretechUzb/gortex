package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
)

// TestResolveHangfireSpawn_ReceiverTyped locks the rail the C# Hangfire
// extraction rides: an EdgeSpawns targeting `unresolved::*.M` with a
// receiver_type hint must land on that type's method, not on a
// same-named method elsewhere.
func TestResolveHangfireSpawn_ReceiverTyped(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "Setup.cs::JobSetup.Configure", Kind: graph.KindMethod, Name: "Configure",
		FilePath: "Setup.cs", Language: "csharp"})
	g.AddNode(&graph.Node{ID: "Sync.cs::SyncService.SynchronizeAsync", Kind: graph.KindMethod, Name: "SynchronizeAsync",
		FilePath: "Sync.cs", Language: "csharp", Meta: map[string]any{"receiver": "SyncService"}})
	g.AddNode(&graph.Node{ID: "Other.cs::OtherService.SynchronizeAsync", Kind: graph.KindMethod, Name: "SynchronizeAsync",
		FilePath: "Other.cs", Language: "csharp", Meta: map[string]any{"receiver": "OtherService"}})

	spawn := &graph.Edge{
		From: "Setup.cs::JobSetup.Configure", To: "unresolved::*.SynchronizeAsync",
		Kind: graph.EdgeSpawns, FilePath: "Setup.cs", Line: 5,
		Meta: map[string]any{"mode": "background", "receiver_type": "SyncService"},
	}
	g.AddEdge(spawn)

	New(g).ResolveAll()

	assert.Equal(t, "Sync.cs::SyncService.SynchronizeAsync", spawn.To,
		"receiver-typed spawn must bind to the generic argument's type, not a same-named method")
}
