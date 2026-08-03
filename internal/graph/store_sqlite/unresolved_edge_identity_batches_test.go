package store_sqlite

import (
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestUnresolvedEdgeIdentityProjectionMatchesGraphAndSkipsPayload(t *testing.T) {
	memory := graph.New()
	sqlite, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlite.Close() })

	seed := func(store graph.Store) {
		store.AddBatch([]*graph.Node{
			{ID: "src", Kind: graph.KindFunction, RepoPrefix: "repo"},
		}, []*graph.Edge{
			{From: "src", To: "unresolved::read", Kind: graph.EdgeReads, FilePath: "a.go", Line: 1, Meta: map[string]any{"payload": strings.Repeat("x", 8192)}},
			{From: "src", To: "repo::unresolved::ref", Kind: graph.EdgeReferences, FilePath: "a.go", Line: 2, Meta: map[string]any{"payload": strings.Repeat("x", 8192)}},
			{From: "src", To: "unresolved::arg", Kind: graph.EdgeArgOf, FilePath: "a.go", Line: 3, Meta: map[string]any{"payload": strings.Repeat("x", 8192)}},
			{From: "src", To: "resolved::value", Kind: graph.EdgeValueFlow, FilePath: "a.go", Line: 4, Meta: map[string]any{"payload": strings.Repeat("x", 8192)}},
			{From: "src", To: "unresolved::call", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5, Meta: map[string]any{"payload": strings.Repeat("x", 8192)}},
		})
	}
	seed(memory)
	seed(sqlite)

	// Full edge hydration now fails; the compact identity projection must remain
	// answerable from its five scalar columns.
	if _, err := sqlite.writerDB.Exec(`UPDATE edges SET meta = x'80'`); err != nil {
		t.Fatal(err)
	}

	kinds := []graph.EdgeKind{
		graph.EdgeReads, graph.EdgeReferences, graph.EdgeArgOf, graph.EdgeValueFlow,
		graph.EdgeReads,
	}
	collect := func(scanner graph.UnresolvedEdgeIdentityBatchScanner, reenter func()) []graph.EdgeIdentity {
		var identities []graph.EdgeIdentity
		scanner.ScanUnresolvedEdgeIdentitiesBatched(kinds, 2, func(page []graph.EdgeIdentity) bool {
			if len(page) == 0 || len(page) > 2 {
				t.Fatalf("page size = %d, want 1..2", len(page))
			}
			if reenter != nil {
				reenter()
			}
			identities = append(identities, page...)
			return true
		})
		sort.Slice(identities, func(i, j int) bool {
			left, right := identities[i], identities[j]
			if left.Kind != right.Kind {
				return left.Kind < right.Kind
			}
			return left.To < right.To
		})
		return identities
	}

	memoryIDs := collect(memory, nil)
	sqliteIDs := collect(sqlite, func() {
		// Every SQLite cursor and the active-write gate must be released before
		// the callback runs.
		if node := sqlite.GetNode("src"); node == nil {
			t.Fatal("store re-entry from identity callback failed")
		}
	})
	if !reflect.DeepEqual(sqliteIDs, memoryIDs) {
		t.Fatalf("projection mismatch:\nsqlite: %#v\nmemory: %#v", sqliteIDs, memoryIDs)
	}
	if len(sqliteIDs) != 3 {
		t.Fatalf("projected identities = %d, want 3: %#v", len(sqliteIDs), sqliteIDs)
	}
	for _, identity := range sqliteIDs {
		if !graph.IsUnresolvedTarget(identity.To) {
			t.Errorf("resolved target leaked into projection: %#v", identity)
		}
	}
}

func TestUnresolvedEdgeIdentityProjectionFreezesHighWaterAcrossCallbackWrite(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{{ID: "src", Kind: graph.KindFunction}}, []*graph.Edge{
		{From: "src", To: "unresolved::one", Kind: graph.EdgeReads, FilePath: "a.go", Line: 1},
		{From: "src", To: "unresolved::two", Kind: graph.EdgeReads, FilePath: "a.go", Line: 2},
	})

	var got []graph.EdgeIdentity
	inserted := false
	store.ScanUnresolvedEdgeIdentitiesBatched(
		[]graph.EdgeKind{graph.EdgeReads},
		1,
		func(page []graph.EdgeIdentity) bool {
			got = append(got, page...)
			if !inserted {
				inserted = true
				store.AddEdge(&graph.Edge{
					From: "src", To: "unresolved::late", Kind: graph.EdgeReads,
					FilePath: "a.go", Line: 3,
				})
			}
			return true
		},
	)
	if len(got) != 2 {
		t.Fatalf("frozen scan yielded %d identities, want 2: %#v", len(got), got)
	}
	for _, identity := range got {
		if identity.To == "unresolved::late" {
			t.Fatal("callback insert crossed the frozen high-water")
		}
	}
}

func BenchmarkSQLiteUnresolvedEdgeIdentityProjection(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "graph.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const edgeCount = 4096
	edges := make([]*graph.Edge, 0, edgeCount)
	payload := strings.Repeat("attribution-does-not-need-this-metadata", 128)
	for i := 0; i < edgeCount; i++ {
		edges = append(edges, &graph.Edge{
			From: "src", To: fmt.Sprintf("unresolved::name-%05d", i),
			Kind: graph.EdgeReferences, FilePath: fmt.Sprintf("file-%05d.go", i), Line: i,
			Meta: map[string]any{"payload": payload},
		})
	}
	store.AddBatch([]*graph.Node{{ID: "src", Kind: graph.KindFunction}}, edges)

	b.Run("full-edge-scan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count := 0
			store.ScanEdgesByKindsBatched(
				[]graph.EdgeKind{graph.EdgeReferences}, 256,
				func(page []*graph.Edge) bool {
					count += len(page)
					return true
				},
			)
			if count != edgeCount {
				b.Fatalf("rows = %d, want %d", count, edgeCount)
			}
		}
	})
	b.Run("bounded-identity-scan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count := 0
			store.ScanUnresolvedEdgeIdentitiesBatched(
				[]graph.EdgeKind{graph.EdgeReferences}, 256,
				func(page []graph.EdgeIdentity) bool {
					count += len(page)
					return true
				},
			)
			if count != edgeCount {
				b.Fatalf("rows = %d, want %d", count, edgeCount)
			}
		}
	})
}
