package store_sqlite

import (
	"fmt"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestBoundedTestProjectionPageSize(t *testing.T) {
	for _, input := range []int{-1, 0} {
		if got := boundedTestProjectionPageSize(input); got != defaultTestProjectionPageSize {
			t.Fatalf("bounded page size(%d) = %d, want %d", input, got, defaultTestProjectionPageSize)
		}
	}
	for _, input := range []int{maxTestProjectionPageSize + 1, 1 << 30} {
		if got := boundedTestProjectionPageSize(input); got != maxTestProjectionPageSize {
			t.Fatalf("bounded page size(%d) = %d, want %d", input, got, maxTestProjectionPageSize)
		}
	}
	if got := boundedTestProjectionPageSize(scopedProjectionPage + 1); got != scopedProjectionPage+1 {
		t.Fatalf("bounded page size(%d) = %d, want independent test-projection cap", scopedProjectionPage+1, got)
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

func newTestProjectionStore(tb testing.TB) *Store {
	tb.Helper()
	store, err := Open(filepath.Join(tb.TempDir(), "graph.db"))
	if err != nil {
		tb.Fatalf("open sqlite store: %v", err)
	}
	tb.Cleanup(func() {
		if err := store.Close(); err != nil {
			tb.Errorf("close sqlite store: %v", err)
		}
	})
	return store
}

func sortTestEdgeProjections(rows []graph.TestEdgeProjection) {
	sort.Slice(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		if left.From != right.From {
			return left.From < right.From
		}
		if left.To != right.To {
			return left.To < right.To
		}
		if left.FilePath != right.FilePath {
			return left.FilePath < right.FilePath
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		return left.Kind < right.Kind
	})
}

func TestScanTestCallProjectionsMatchesWholeCallFilter(t *testing.T) {
	store := newTestProjectionStore(t)
	store.AddBatch(nil, []*graph.Edge{
		{From: "test/a", To: "prod/one", Kind: graph.EdgeCalls, FilePath: "a_test.go", Line: 10},
		{From: "prod/helper", To: "prod/one", Kind: graph.EdgeCalls, FilePath: "helper.go", Line: 20},
		{From: "test/b", To: "test/a", Kind: graph.EdgeCalls, FilePath: "b_test.go", Line: 30},
		{From: "test/a", To: "prod/two", Kind: graph.EdgeCalls, FilePath: "a_test.go", Line: 40},
		{From: "test/b", To: "prod/two", Kind: graph.EdgeCalls, FilePath: "b_test.go", Line: 50},
		{From: "test/a", To: "annotation", Kind: graph.EdgeAnnotated, FilePath: "a_test.go", Line: 60},
	})

	testSources := map[string]bool{"test/a": true, "test/b": true}
	var want []graph.TestEdgeProjection
	store.ScanTestEdgeProjections([]graph.EdgeKind{graph.EdgeCalls}, 2, func(rows []graph.TestEdgeProjection) bool {
		for _, row := range rows {
			if testSources[row.From] {
				want = append(want, row)
			}
		}
		return true
	})

	var got []graph.TestEdgeProjection
	store.ScanTestCallProjections([]string{"test/b", "", "missing", "test/a", "test/b"}, 1, 1, func(rows []graph.TestEdgeProjection) bool {
		if len(rows) > 1 {
			t.Fatalf("projection page has %d rows, want at most 1", len(rows))
		}
		got = append(got, rows...)
		return true
	})

	sortTestEdgeProjections(want)
	sortTestEdgeProjections(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("source projection differs from whole-kind filter:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestScanTestCallProjectionsFreezesHighWaterAndAllowsReentry(t *testing.T) {
	store := newTestProjectionStore(t)
	store.AddBatch(nil, []*graph.Edge{
		{From: "test/a", To: "prod/one", Kind: graph.EdgeCalls, FilePath: "a_test.go", Line: 10},
		{From: "test/b", To: "prod/two", Kind: graph.EdgeCalls, FilePath: "b_test.go", Line: 20},
	})

	firstPage := true
	var got []graph.TestEdgeProjection
	store.ScanTestCallProjections([]string{"test/b", "test/a"}, 1, 1, func(rows []graph.TestEdgeProjection) bool {
		got = append(got, rows...)
		if firstPage {
			firstPage = false
			if out := store.GetOutEdges("test/a"); len(out) != 1 {
				t.Fatalf("re-entrant adjacency read returned %d edges, want 1", len(out))
			}
			store.AddBatch(nil, []*graph.Edge{{
				From: "test/b", To: "prod/late", Kind: graph.EdgeCalls, FilePath: "b_test.go", Line: 99,
			}})
		}
		return true
	})

	sortTestEdgeProjections(got)
	if len(got) != 2 || got[0].Line != 10 || got[1].Line != 20 {
		t.Fatalf("scan observed callback write or lost initial rows: %#v", got)
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

func BenchmarkTestCallSourceProjection(b *testing.B) {
	store := newTestProjectionStore(b)
	const (
		sourceCount  = 10_000
		callsPerNode = 5
		selected     = 64
	)
	edges := make([]*graph.Edge, 0, sourceCount*callsPerNode)
	for source := 0; source < sourceCount; source++ {
		from := fmt.Sprintf("source/%05d", source)
		for call := 0; call < callsPerNode; call++ {
			edges = append(edges, &graph.Edge{
				From: from, To: fmt.Sprintf("target/%02d", call), Kind: graph.EdgeCalls,
				FilePath: fmt.Sprintf("repo/file_%05d.go", source), Line: call + 1,
			})
		}
	}
	store.AddBatch(nil, edges)

	sourceIDs := make([]string, 0, selected)
	selectedSet := make(map[string]bool, selected)
	for source := 0; source < selected; source++ {
		id := fmt.Sprintf("source/%05d", source)
		sourceIDs = append(sourceIDs, id)
		selectedSet[id] = true
	}

	b.Run("whole_calls_then_filter", func(b *testing.B) {
		b.ReportAllocs()
		var candidates, matched int
		b.ResetTimer()
		for range b.N {
			store.ScanTestEdgeProjections([]graph.EdgeKind{graph.EdgeCalls}, 4096, func(rows []graph.TestEdgeProjection) bool {
				candidates += len(rows)
				for _, row := range rows {
					if selectedSet[row.From] {
						matched++
					}
				}
				return true
			})
		}
		b.ReportMetric(float64(candidates)/float64(b.N), "candidate_rows/op")
		b.ReportMetric(float64(matched)/float64(b.N), "matched_rows/op")
		runtime.KeepAlive(matched)
	})

	b.Run("known_test_sources", func(b *testing.B) {
		b.ReportAllocs()
		var candidates int
		b.ResetTimer()
		for range b.N {
			store.ScanTestCallProjections(sourceIDs, 512, 4096, func(rows []graph.TestEdgeProjection) bool {
				candidates += len(rows)
				return true
			})
		}
		b.ReportMetric(float64(candidates)/float64(b.N), "candidate_rows/op")
		runtime.KeepAlive(candidates)
	})
}
