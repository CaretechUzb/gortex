package indexer

import (
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestMarkTestSymbolsAndEmitEdges_SQLiteProjectionMatchesFallback(t *testing.T) {
	type result struct {
		marked  int
		emitted int
		meta    map[string]map[string]any
		edges   []string
	}
	run := func(t *testing.T, store graph.Store) result {
		t.Helper()
		store.AddBatch([]*graph.Node{
			{ID: "src/app.go", Kind: graph.KindFile, Name: "src/app.go", FilePath: "src/app.go", Language: "go"},
			{ID: "src/app_test.go", Kind: graph.KindFile, Name: "src/app_test.go", FilePath: "src/app_test.go", Language: "go"},
			{ID: "src/lib.rs", Kind: graph.KindFile, Name: "src/lib.rs", FilePath: "src/lib.rs", Language: "rust"},
			{ID: "src/app.go::Subject", Kind: graph.KindFunction, Name: "Subject", FilePath: "src/app.go", Language: "go"},
			{ID: "src/app_test.go::TestSubject", Kind: graph.KindFunction, Name: "TestSubject", FilePath: "src/app_test.go", Language: "go"},
			{ID: "src/app_test.go::setup", Kind: graph.KindFunction, Name: "setup", FilePath: "src/app_test.go", Language: "go"},
			{ID: "src/lib.rs::it_works", Kind: graph.KindFunction, Name: "it_works", FilePath: "src/lib.rs", Language: "rust"},
			{ID: "annotation::rust::test", Kind: graph.KindType, Name: "test", FilePath: "src/lib.rs", Language: "rust"},
		}, []*graph.Edge{
			{From: "src/lib.rs::it_works", To: "annotation::rust::test", Kind: graph.EdgeAnnotated, FilePath: "src/lib.rs", Line: 3},
			{From: "src/app_test.go::TestSubject", To: "src/app.go::Subject", Kind: graph.EdgeCalls, FilePath: "src/app_test.go", Line: 5},
			{From: "src/app_test.go::TestSubject", To: "src/app.go::Subject", Kind: graph.EdgeCalls, FilePath: "src/app_test.go", Line: 9},
			{From: "src/app_test.go::TestSubject", To: "src/app_test.go::setup", Kind: graph.EdgeCalls, FilePath: "src/app_test.go", Line: 10},
			{From: "src/lib.rs::it_works", To: "src/app.go::Subject", Kind: graph.EdgeCalls, FilePath: "src/lib.rs", Line: 4},
		})

		marked, emitted := markTestSymbolsAndEmitEdges(store)
		ids := []string{
			"src/app_test.go",
			"src/app_test.go::TestSubject",
			"src/app_test.go::setup",
			"src/lib.rs::it_works",
		}
		meta := make(map[string]map[string]any, len(ids))
		for _, id := range ids {
			node := store.GetNode(id)
			if node == nil {
				t.Fatalf("node %q missing", id)
			}
			meta[id] = node.Meta
		}
		var edges []string
		for edge := range store.EdgesByKind(graph.EdgeTests) {
			if edge != nil {
				edges = append(edges, fmt.Sprintf("%s>%s@%s:%d", edge.From, edge.To, edge.FilePath, edge.Line))
			}
		}
		slices.Sort(edges)
		return result{marked: marked, emitted: emitted, meta: meta, edges: edges}
	}

	fallback := run(t, graph.New())
	sqliteStore, err := store_sqlite.Open(filepath.Join(t.TempDir(), "projection.sqlite"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	defer func() { _ = sqliteStore.Close() }()
	projected := run(t, sqliteStore)

	if !reflect.DeepEqual(projected, fallback) {
		t.Fatalf("projected result differs from fallback\nprojected: %#v\nfallback:  %#v", projected, fallback)
	}
	if projected.marked != 3 || projected.emitted != 2 {
		t.Fatalf("mark/emit counts = %d/%d, want 3/2", projected.marked, projected.emitted)
	}
}
