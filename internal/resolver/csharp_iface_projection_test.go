package resolver

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// csharpProjectionLegacyStore deliberately hides optional streaming
// capabilities while preserving the required graph.Store contract. It keeps
// the pre-projection scoped path available as a byte-for-byte parity oracle.
type csharpProjectionLegacyStore struct {
	graph.Store
}

func TestCSharpScopedMemberProjectionMatchesLegacy(t *testing.T) {
	projected := newCSharpProjectionStore(t, "projected")
	legacy := newCSharpProjectionStore(t, "legacy")
	seedCSharpProjectionStore(projected, 32)
	seedCSharpProjectionStore(legacy, 32)

	scope := map[string]bool{"cs": true}
	projectedCount := ResolveCSharpInterfaceDispatchScoped(projected, scope)
	legacyCount := ResolveCSharpInterfaceDispatchScoped(&csharpProjectionLegacyStore{Store: legacy}, scope)
	if projectedCount != legacyCount {
		t.Fatalf("dispatch count mismatch: projected=%d legacy=%d", projectedCount, legacyCount)
	}

	projectedEdges := csharpProjectionDispatchSnapshot(projected)
	legacyEdges := csharpProjectionDispatchSnapshot(legacy)
	if fmt.Sprint(projectedEdges) != fmt.Sprint(legacyEdges) {
		t.Fatalf("dispatch edge mismatch:\nprojected=%v\nlegacy=%v", projectedEdges, legacyEdges)
	}
	if projectedCount != 2 || len(projectedEdges) != 2 {
		t.Fatalf("expected two implementation fan-out edges, count=%d edges=%v", projectedCount, projectedEdges)
	}
}

func BenchmarkCSharpScopedMemberProjection(b *testing.B) {
	const noiseMethods = 4000
	scope := map[string]bool{"cs": true}

	b.Run("legacy_full_decode", func(b *testing.B) {
		store := newCSharpProjectionStore(b, "legacy")
		seedCSharpProjectionStore(store, noiseMethods)
		wrapped := &csharpProjectionLegacyStore{Store: store}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ResolveCSharpInterfaceDispatchScoped(wrapped, scope)
		}
	})

	b.Run("projected", func(b *testing.B) {
		store := newCSharpProjectionStore(b, "projected")
		seedCSharpProjectionStore(store, noiseMethods)
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ResolveCSharpInterfaceDispatchScoped(store, scope)
		}
	})
}

func newCSharpProjectionStore(tb testing.TB, name string) *store_sqlite.Store {
	tb.Helper()
	store, err := store_sqlite.Open(filepath.Join(tb.TempDir(), name+".sqlite"))
	if err != nil {
		tb.Fatalf("open SQLite store: %v", err)
	}
	tb.Cleanup(func() {
		if err := store.Close(); err != nil {
			tb.Errorf("close SQLite store: %v", err)
		}
	})
	return store
}

func seedCSharpProjectionStore(store graph.Store, noiseMethods int) {
	const (
		ifaceType   = "cs::IService"
		ifaceMethod = "cs::IService.Run"
		implAType   = "cs::ServiceA"
		implAMethod = "cs::ServiceA.Run"
		implBType   = "cs::ServiceB"
		implBMethod = "cs::ServiceB.Run"
		caller      = "cs::Caller.Call"
	)

	nodes := []*graph.Node{
		{ID: ifaceType, Kind: graph.KindInterface, Name: "IService", Language: "csharp", FilePath: "IService.cs", RepoPrefix: "cs"},
		{ID: ifaceMethod, Kind: graph.KindMethod, Name: "Run", Language: "csharp", FilePath: "IService.cs", RepoPrefix: "cs", Meta: map[string]any{"iface_member": true, "payload": strings.Repeat("i", 2048)}},
		{ID: implAType, Kind: graph.KindType, Name: "ServiceA", Language: "csharp", FilePath: "ServiceA.cs", RepoPrefix: "cs"},
		{ID: implAMethod, Kind: graph.KindMethod, Name: "Run", Language: "csharp", FilePath: "ServiceA.cs", RepoPrefix: "cs"},
		{ID: implBType, Kind: graph.KindType, Name: "ServiceB", Language: "csharp", FilePath: "ServiceB.cs", RepoPrefix: "cs"},
		{ID: implBMethod, Kind: graph.KindMethod, Name: "Run", Language: "csharp", FilePath: "ServiceB.cs", RepoPrefix: "cs"},
		{ID: caller, Kind: graph.KindMethod, Name: "Call", Language: "csharp", FilePath: "Caller.cs", RepoPrefix: "cs"},
	}
	edges := []*graph.Edge{
		{From: ifaceMethod, To: ifaceType, Kind: graph.EdgeMemberOf, FilePath: "IService.cs", Line: 2},
		{From: implAMethod, To: implAType, Kind: graph.EdgeMemberOf, FilePath: "ServiceA.cs", Line: 2},
		{From: implBMethod, To: implBType, Kind: graph.EdgeMemberOf, FilePath: "ServiceB.cs", Line: 2},
		{From: implAType, To: ifaceType, Kind: graph.EdgeImplements, FilePath: "ServiceA.cs", Line: 1},
		{From: implBType, To: ifaceType, Kind: graph.EdgeImplements, FilePath: "ServiceB.cs", Line: 1},
		{From: caller, To: ifaceMethod, Kind: graph.EdgeCalls, FilePath: "Caller.cs", Line: 3},
	}

	payload := strings.Repeat("n", 2048)
	for i := 0; i < noiseMethods; i++ {
		typeID := fmt.Sprintf("cs::NoiseType%05d", i)
		methodID := typeID + ".Run"
		path := fmt.Sprintf("Noise%05d.cs", i)
		nodes = append(nodes,
			&graph.Node{ID: typeID, Kind: graph.KindType, Name: fmt.Sprintf("NoiseType%05d", i), Language: "csharp", FilePath: path, RepoPrefix: "cs"},
			&graph.Node{ID: methodID, Kind: graph.KindMethod, Name: "Run", Language: "csharp", FilePath: path, RepoPrefix: "cs", Meta: map[string]any{"payload": payload}},
		)
		edges = append(edges, &graph.Edge{
			From: methodID, To: typeID, Kind: graph.EdgeMemberOf,
			FilePath: path, Line: 2, Meta: map[string]any{"payload": payload},
		})
	}
	store.AddBatch(nodes, edges)
}

func csharpProjectionDispatchSnapshot(store graph.Store) []string {
	targets := []string{"cs::ServiceA.Run", "cs::ServiceB.Run"}
	incoming := store.GetInEdgesByNodeIDs(targets)
	var snapshot []string
	for _, target := range targets {
		for _, edge := range incoming[target] {
			if edge == nil || edge.Kind != graph.EdgeCalls || edge.Meta == nil || edge.Meta[MetaSynthesizedBy] != SynthCSharpIfaceDispatch {
				continue
			}
			meta, _ := json.Marshal(edge.Meta)
			snapshot = append(snapshot, fmt.Sprintf("%s|%s|%s|%d|%s|%s", edge.From, edge.To, edge.FilePath, edge.Line, edge.Origin, meta))
		}
	}
	sort.Strings(snapshot)
	return snapshot
}
