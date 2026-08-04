package store_sqlite

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestSQLiteCapabilityProjectionWholeGraphFreezesHighWaterAndDistinguishesEmptyScope(t *testing.T) {
	store, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "a::source", Kind: graph.KindFunction, RepoPrefix: "a"},
		{ID: "b::source", Kind: graph.KindFunction, RepoPrefix: "b"},
	}, []*graph.Edge{
		{From: "a::source", To: "proc::a", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1},
		{From: "b::source", To: "proc::b", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 2},
	})

	var got []graph.RepoCapabilityEdge
	store.ScanRepoCapabilityEdges(nil, 1, func(page []graph.RepoCapabilityEdge) bool {
		got = append(got, page...)
		if len(got) == 1 {
			// yield runs after Rows.Close, so this write must not deadlock. Its
			// row ID is beyond the frozen high-water mark and stays for next run.
			store.AddEdge(&graph.Edge{
				From: "a::source", To: "proc::late", Kind: graph.EdgeCalls,
				FilePath: "late.go", Line: 3,
			})
		}
		return true
	})
	if len(got) != 2 {
		t.Fatalf("whole-graph rows = %d, want frozen 2: %#v", len(got), got)
	}
	seenRepos := map[string]bool{}
	for _, row := range got {
		seenRepos[row.RepoPrefix] = true
		if row.Identity.To == "proc::late" {
			t.Fatal("row inserted after high-water mark leaked into scan")
		}
	}
	if !seenRepos["a"] || !seenRepos["b"] {
		t.Fatalf("whole-graph repositories = %#v, want a and b", seenRepos)
	}

	callbacks := 0
	store.ScanRepoCapabilityEdges([]string{}, 16, func([]graph.RepoCapabilityEdge) bool {
		callbacks++
		return true
	})
	if callbacks != 0 {
		t.Fatalf("exact empty scope yielded %d pages, want 0", callbacks)
	}
}
