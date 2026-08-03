package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSQLiteCapabilityProjectionIsBoundedMetadataFreeAndReentrant(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.AddBatch([]*graph.Node{
		{ID: "a-src", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "b-src", Kind: graph.KindFunction, RepoPrefix: "b"},
		{ID: "field", Kind: graph.KindField, RepoPrefix: "definitions"},
		{ID: "not-field", Kind: graph.KindFunction, RepoPrefix: "definitions"},
	}, []*graph.Edge{
		{From: "a-src", To: "field", Kind: graph.EdgeReads, FilePath: "a.go", Line: 1, Meta: map[string]any{"payload": strings.Repeat("x", 8192)}},
		{From: "a-src", To: "not-field", Kind: graph.EdgeWrites, FilePath: "a.go", Line: 2, Meta: map[string]any{"payload": strings.Repeat("x", 8192)}},
		{From: "a-src", To: "cfg::env::TOKEN", Kind: graph.EdgeReadsConfig, FilePath: "a.go", Line: 3, Meta: map[string]any{"payload": strings.Repeat("x", 8192)}},
		{From: "a-src", To: "unresolved::exec.Command", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 4, Meta: map[string]any{"payload": strings.Repeat("x", 8192)}},
		{From: "b-src", To: "field", Kind: graph.EdgeReads, FilePath: "b.go", Line: 5, Meta: map[string]any{"payload": strings.Repeat("x", 8192)}},
	})

	// Make full Edge hydration fail. The compact projection must remain fully
	// answerable from scalar identity columns and the target kind join.
	if _, err := store.writerDB.Exec(`UPDATE edges SET meta = x'80'`); err != nil {
		t.Fatal(err)
	}

	var got []graph.RepoCapabilityEdge
	callbacks := 0
	store.ScanRepoCapabilityEdges([]string{"a", "a"}, 1, func(page []graph.RepoCapabilityEdge) bool {
		callbacks++
		if len(page) != 1 {
			t.Fatalf("page size = %d, want 1", len(page))
		}
		// The page cursor must already be closed when yield runs.
		if node := store.GetNode("a-src"); node == nil {
			t.Fatal("store re-entry from projection callback failed")
		}
		got = append(got, page...)
		return true
	})
	if callbacks != 3 {
		t.Fatalf("callbacks = %d, want 3", callbacks)
	}
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3: %#v", len(got), got)
	}

	want := map[graph.EdgeKind]bool{
		graph.EdgeReads:       false,
		graph.EdgeReadsConfig: false,
		graph.EdgeCalls:       false,
	}
	for _, row := range got {
		if row.RepoPrefix != "a" {
			t.Fatalf("repo prefix = %q, want a", row.RepoPrefix)
		}
		if _, ok := want[row.Identity.Kind]; !ok {
			t.Fatalf("unexpected projected edge: %#v", row)
		}
		want[row.Identity.Kind] = true
	}
	for kind, seen := range want {
		if !seen {
			t.Errorf("missing %s projection", kind)
		}
	}
}

func BenchmarkSQLiteCapabilityProjection(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "graph.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	const edgeCount = 4096
	nodes := []*graph.Node{
		{ID: "src", Kind: graph.KindFunction, RepoPrefix: "repo"},
		{ID: "field", Kind: graph.KindField, RepoPrefix: "definitions"},
	}
	edges := make([]*graph.Edge, 0, edgeCount)
	payload := strings.Repeat("metadata-not-needed-by-capability-synthesis", 128)
	for i := 0; i < edgeCount; i++ {
		edges = append(edges, &graph.Edge{
			From: "src", To: "field", Kind: graph.EdgeReads,
			FilePath: fmt.Sprintf("file-%05d.go", i), Line: i,
			Meta: map[string]any{"payload": payload},
		})
	}
	store.AddBatch(nodes, edges)

	b.Run("full-edge-materialization", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if rows := store.RepoEdgesByKinds([]string{"repo"}, []graph.EdgeKind{graph.EdgeReads}); len(rows) != edgeCount {
				b.Fatalf("rows = %d, want %d", len(rows), edgeCount)
			}
		}
	})
	b.Run("bounded-identity-projection", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count := 0
			store.ScanRepoCapabilityEdges([]string{"repo"}, 256, func(page []graph.RepoCapabilityEdge) bool {
				count += len(page)
				return true
			})
			if count != edgeCount {
				b.Fatalf("rows = %d, want %d", count, edgeCount)
			}
		}
	})
	b.Run("whole-graph-default-projection", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count := 0
			graph.ScanRepoCapabilityEdges(store, nil, 0, func(page []graph.RepoCapabilityEdge) bool {
				count += len(page)
				return true
			})
			if count != edgeCount {
				b.Fatalf("rows = %d, want %d", count, edgeCount)
			}
		}
	})
}
