package store_sqlite

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestBoundedTestProjectionPageSize(t *testing.T) {
	for _, input := range []int{-1, 0, scopedProjectionPage + 1, 1 << 30} {
		if got := boundedTestProjectionPageSize(input); got != defaultTestProjectionPageSize {
			t.Fatalf("bounded page size(%d) = %d, want %d", input, got, defaultTestProjectionPageSize)
		}
	}
	if got := boundedTestProjectionPageSize(17); got != 17 {
		t.Fatalf("bounded page size(17) = %d, want 17", got)
	}
}

func TestScanTestProjectionsPageAndFreezeHighWater(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "projection.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()

	// Nil callbacks are a supported no-op rather than a latent panic in the
	// optional projection surface.
	store.ScanTestNodeProjections([]graph.NodeKind{graph.KindFunction}, 1, nil)
	store.ScanTestEdgeProjections([]graph.EdgeKind{graph.EdgeCalls}, 1, nil)

	store.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", Language: "go", Meta: map[string]any{"opaque": strings.Repeat("a", 2048)}},
		{ID: "b", Kind: graph.KindFunction, Name: "B", FilePath: "b.go", Language: "go", Meta: map[string]any{"opaque": strings.Repeat("b", 2048)}},
		{ID: "c", Kind: graph.KindFunction, Name: "C", FilePath: "c.go", Language: "go", Meta: map[string]any{"opaque": strings.Repeat("c", 2048)}},
		{ID: "file", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"},
	}, []*graph.Edge{
		{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1, Meta: map[string]any{"opaque": strings.Repeat("x", 2048)}},
		{From: "b", To: "c", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 2, Meta: map[string]any{"opaque": strings.Repeat("y", 2048)}},
		{From: "a", To: "annotation::go::test", Kind: graph.EdgeAnnotated, FilePath: "a.go", Line: 3},
	})

	var nodeIDs []string
	var nodePageSizes []int
	store.ScanTestNodeProjections([]graph.NodeKind{graph.KindFunction}, 2, func(rows []graph.TestNodeProjection) bool {
		nodePageSizes = append(nodePageSizes, len(rows))
		for _, row := range rows {
			if row.Kind != graph.KindFunction {
				t.Fatalf("node kind = %q, want function", row.Kind)
			}
			nodeIDs = append(nodeIDs, row.ID)
		}
		if len(nodePageSizes) == 1 {
			// The scanner contract closes its cursor before yielding. Re-entering
			// the store must not deadlock, and the frozen high-water excludes z.
			store.AddNode(&graph.Node{ID: "z", Kind: graph.KindFunction, Name: "Z", FilePath: "z.go", Language: "go"})
		}
		return true
	})
	if !slices.Equal(nodeIDs, []string{"a", "b", "c"}) {
		t.Fatalf("node IDs = %v, want [a b c]", nodeIDs)
	}
	if !slices.Equal(nodePageSizes, []int{2, 1}) {
		t.Fatalf("node page sizes = %v, want [2 1]", nodePageSizes)
	}

	var callRows []graph.TestEdgeProjection
	var edgePageSizes []int
	store.ScanTestEdgeProjections([]graph.EdgeKind{graph.EdgeCalls}, 1, func(rows []graph.TestEdgeProjection) bool {
		edgePageSizes = append(edgePageSizes, len(rows))
		callRows = append(callRows, rows...)
		if len(edgePageSizes) == 1 {
			store.AddEdge(&graph.Edge{From: "c", To: "a", Kind: graph.EdgeCalls, FilePath: "c.go", Line: 4})
		}
		return true
	})
	if len(callRows) != 2 {
		t.Fatalf("call projection rows = %d, want 2", len(callRows))
	}
	if !slices.Equal(edgePageSizes, []int{1, 1}) {
		t.Fatalf("edge page sizes = %v, want [1 1]", edgePageSizes)
	}
	for _, row := range callRows {
		if row.Kind != graph.EdgeCalls {
			t.Fatalf("edge kind = %q, want calls", row.Kind)
		}
		if row.From == "c" && row.To == "a" {
			t.Fatal("edge inserted after the high-water mark leaked into scan")
		}
	}
}

func BenchmarkTestCallEdgeProjection(b *testing.B) {
	store, err := Open(filepath.Join(b.TempDir(), "projection-bench.sqlite"))
	if err != nil {
		b.Fatalf("open store: %v", err)
	}
	b.Cleanup(func() { _ = store.Close() })

	const rowCount = 5000
	nodes := make([]*graph.Node, 0, rowCount)
	edges := make([]*graph.Edge, 0, rowCount-1)
	payload := strings.Repeat("metadata", 128)
	for i := 0; i < rowCount; i++ {
		id := fmt.Sprintf("n%06d", i)
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: id,
			FilePath: "bench.go", Language: "go",
		})
		if i > 0 {
			edges = append(edges, &graph.Edge{
				From: fmt.Sprintf("n%06d", i-1), To: id,
				Kind: graph.EdgeCalls, FilePath: "bench.go", Line: i,
				Meta: map[string]any{"opaque": payload},
			})
		}
	}
	store.AddBatch(nodes, edges)

	b.Run("typed_keyset", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count := 0
			store.ScanTestEdgeProjections([]graph.EdgeKind{graph.EdgeCalls}, 256, func(rows []graph.TestEdgeProjection) bool {
				count += len(rows)
				return true
			})
			if count != rowCount-1 {
				b.Fatalf("rows = %d, want %d", count, rowCount-1)
			}
		}
		b.ReportMetric(rowCount-1, "rows/op")
	})
	b.Run("full_edge_decode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			count := 0
			for edge := range store.EdgesByKind(graph.EdgeCalls) {
				if edge != nil {
					count++
				}
			}
			if count != rowCount-1 {
				b.Fatalf("rows = %d, want %d", count, rowCount-1)
			}
		}
		b.ReportMetric(rowCount-1, "rows/op")
	})
}
