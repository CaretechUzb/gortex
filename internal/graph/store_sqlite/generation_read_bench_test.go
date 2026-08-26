package store_sqlite

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// BenchmarkGenerationScopedReads measures the invariant the derived-view
// architecture relies on: reading a small generation must scale with that
// generation, not with generation zero beside it in the shared tables.
func BenchmarkGenerationScopedReads(b *testing.B) {
	const (
		baseRows       = 30_000
		generationRows = 32
	)
	_, generation := newGenerationReadBenchmarkStore(b, baseRows, generationRows)
	b.ReportMetric(baseRows, "base_rows")
	b.ReportMetric(generationRows, "generation_rows")

	b.Run("AllNodes", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if got := len(generation.AllNodes()); got != generationRows+1 {
				b.Fatalf("AllNodes() = %d rows, want %d", got, generationRows+1)
			}
		}
	})
	b.Run("AllEdges", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if got := len(generation.AllEdges()); got != generationRows {
				b.Fatalf("AllEdges() = %d rows, want %d", got, generationRows)
			}
		}
	})
	b.Run("NodesByKind", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			got := 0
			for range generation.NodesByKind(graph.KindMethod) {
				got++
			}
			if got != generationRows {
				b.Fatalf("NodesByKind(method) = %d rows, want %d", got, generationRows)
			}
		}
	})
	b.Run("EdgesByKind", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			got := 0
			for range generation.EdgesByKind(graph.EdgeMemberOf) {
				got++
			}
			if got != generationRows {
				b.Fatalf("EdgesByKind(member_of) = %d rows, want %d", got, generationRows)
			}
		}
	})
	b.Run("MemberMethodsByType", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			got := generation.MemberMethodsByType()
			if methods := len(got["generation::type"]); methods != generationRows {
				b.Fatalf("MemberMethodsByType()[generation::type] = %d rows, want %d", methods, generationRows)
			}
		}
	})
}

func newGenerationReadBenchmarkStore(tb testing.TB, baseRows, generationRows int) (*Store, *Store) {
	tb.Helper()
	store, err := Open(":memory:")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = store.Close() })

	addMemberRows(tb, store, "base", baseRows)
	_, generation, _, err := store.BeginPayloadGenerationWithStatus(context.Background(), PayloadGenerationRequest{
		OwnerKind:      "benchmark",
		GraphID:        "benchmark-graph",
		LayerID:        "benchmark-layer",
		GenerationKind: "dirty",
		CreatedAt:      1,
	})
	if err != nil {
		tb.Fatal(err)
	}
	addMemberRows(tb, generation, "generation", generationRows)
	return store, generation
}

func addMemberRows(tb testing.TB, store *Store, prefix string, count int) {
	tb.Helper()
	typeID := prefix + "::type"
	store.AddNode(&graph.Node{ID: typeID, Name: "Type", Kind: graph.KindType, FilePath: prefix + "/type.go"})
	const chunkSize = 1_000
	for start := 0; start < count; start += chunkSize {
		end := min(start+chunkSize, count)
		nodes := make([]*graph.Node, 0, end-start)
		edges := make([]*graph.Edge, 0, end-start)
		for i := start; i < end; i++ {
			methodID := fmt.Sprintf("%s::method::%06d", prefix, i)
			filePath := fmt.Sprintf("%s/method_%06d.go", prefix, i)
			nodes = append(nodes, &graph.Node{
				ID: methodID, Name: "Method", Kind: graph.KindMethod, FilePath: filePath,
			})
			edges = append(edges, &graph.Edge{
				From: methodID, To: typeID, Kind: graph.EdgeMemberOf, FilePath: filePath, Line: i + 1,
			})
		}
		store.AddBatch(nodes, edges)
	}
}
