package store_sqlite

import (
	"bytes"
	"encoding/gob"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestDecodeOverrideDispatchSpeculativeFormats(t *testing.T) {
	t.Parallel()
	flat, err := encodeMeta(map[string]any{
		graph.MetaSpeculative: true,
		"ignored":             "payload",
	})
	if err != nil {
		t.Fatal(err)
	}
	jsonBlob, err := encodeMeta(map[string]any{
		graph.MetaSpeculative: true,
		"unsupported":         []int{1, 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	if isFlatMeta(jsonBlob) {
		t.Fatal("unsupported value did not force JSON fallback")
	}
	var gobBuf bytes.Buffer
	if err := gob.NewEncoder(&gobBuf).Encode(map[string]any{
		graph.MetaSpeculative: true,
	}); err != nil {
		t.Fatal(err)
	}

	for name, blob := range map[string][]byte{
		"flat": flat,
		"json": jsonBlob,
		"gob":  gobBuf.Bytes(),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := decodeOverrideDispatchSpeculative(blob)
			if err != nil {
				t.Fatal(err)
			}
			if !got {
				t.Fatal("speculative metadata was not recognized")
			}
		})
	}

	wrongType, err := encodeMeta(map[string]any{graph.MetaSpeculative: "true"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := decodeOverrideDispatchSpeculative(wrongType)
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Fatal("non-bool speculative metadata was accepted")
	}
	if _, err := decodeOverrideDispatchSpeculative(flat[:len(flat)-1]); err == nil {
		t.Fatal("truncated flat metadata was accepted")
	}
}

func TestOverrideDispatchCallProjectionGraphSQLiteParity(t *testing.T) {
	sqliteStore, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer sqliteStore.Close()
	memoryStore := graph.New()

	for _, store := range []graph.Store{memoryStore, sqliteStore} {
		seedOverrideDispatchProjection(store)
	}

	memoryAll := collectOverrideDispatchRows(memoryStore, nil, 1)
	sqliteAll := collectOverrideDispatchRows(sqliteStore, nil, 1)
	if !reflect.DeepEqual(sqliteAll, memoryAll) {
		t.Fatalf("SQLite rows %#v, in-memory rows %#v", sqliteAll, memoryAll)
	}
	if len(sqliteAll) != 2 || sqliteAll[0].CallerLanguage != "java" || sqliteAll[1].CallerLanguage != "php" {
		t.Fatalf("unexpected projected rows: %#v", sqliteAll)
	}

	memoryScoped := collectOverrideDispatchRows(memoryStore, []string{"repo-a"}, 1)
	sqliteScoped := collectOverrideDispatchRows(sqliteStore, []string{"repo-a", "repo-a"}, 1)
	if !reflect.DeepEqual(sqliteScoped, memoryScoped) || len(sqliteScoped) != 1 || sqliteScoped[0].CallerLanguage != "java" {
		t.Fatalf("scoped parity mismatch: SQLite=%#v memory=%#v", sqliteScoped, memoryScoped)
	}

	finder := any(sqliteStore).(graph.EdgeIdentityBatchFinder)
	for _, row := range sqliteAll {
		if edge := finder.FindEdgesByIdentities([]graph.EdgeIdentity{row.EdgeIdentity})[row.EdgeIdentity]; edge == nil {
			t.Fatalf("projected identity did not exact-refetch: %#v", row.EdgeIdentity)
		}
	}
}

func TestOverrideDispatchCallProjectionClosesPagesAndFreezesHighWater(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	caller := &graph.Node{ID: "repo::Caller.run", Kind: graph.KindMethod, Language: "java", RepoPrefix: "repo"}
	s.AddNode(caller)
	for _, line := range []int{1, 2} {
		s.AddEdge(&graph.Edge{From: caller.ID, To: "unresolved::*.run", Kind: graph.EdgeCalls, FilePath: "a.java", Line: line})
	}

	var got []graph.OverrideDispatchCall
	inserted := false
	s.ScanOverrideDispatchCalls(nil, 1, func(page []graph.OverrideDispatchCall) bool {
		got = append(got, page...)
		if !inserted {
			inserted = true
			s.AddEdge(&graph.Edge{From: caller.ID, To: "unresolved::*.run", Kind: graph.EdgeCalls, FilePath: "a.java", Line: 3})
		}
		return true
	})
	if len(got) != 2 {
		t.Fatalf("frozen scan returned %d rows, want 2: %#v", len(got), got)
	}
	if next := collectOverrideDispatchRows(s, nil, 1); len(next) != 3 {
		t.Fatalf("following scan returned %d rows, want 3", len(next))
	}

	s.ScanOverrideDispatchCalls(nil, 1, func([]graph.OverrideDispatchCall) bool { return false })
	s.AddEdge(&graph.Edge{From: caller.ID, To: "unresolved::*.after", Kind: graph.EdgeCalls, FilePath: "a.java", Line: 4})
	if len(s.GetOutEdges(caller.ID)) != 4 {
		t.Fatal("early-stop cursor was not closed before the following write")
	}
}

func seedOverrideDispatchProjection(store graph.Store) {
	nodes := []*graph.Node{
		{ID: "repo-a/a.java::Java.run", Kind: graph.KindMethod, Language: "java", RepoPrefix: "repo-a"},
		{ID: "repo-b/b.php::PHP.run", Kind: graph.KindMethod, Language: "php", RepoPrefix: "repo-b"},
		{ID: "repo-a/a.go::Go.run", Kind: graph.KindMethod, Language: "go", RepoPrefix: "repo-a"},
	}
	edges := []*graph.Edge{
		{From: nodes[0].ID, To: "unresolved::*.call", Kind: graph.EdgeCalls, FilePath: "a.java", Line: 1},
		{From: nodes[1].ID, To: "repo-b::unresolved::*.call", Kind: graph.EdgeCalls, FilePath: "b.php", Line: 2},
		{From: nodes[0].ID, To: "resolved::call", Kind: graph.EdgeCalls, FilePath: "a.java", Line: 3},
		{From: nodes[0].ID, To: "unresolved::*.hidden", Kind: graph.EdgeCalls, FilePath: "a.java", Line: 4, Meta: map[string]any{graph.MetaSpeculative: true}},
		{From: nodes[2].ID, To: "unresolved::*.call", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 5},
		{From: nodes[0].ID, To: "unresolved::*.ref", Kind: graph.EdgeReferences, FilePath: "a.java", Line: 6},
		{From: "repo-a/a.java::missing", To: "unresolved::*.missing", Kind: graph.EdgeCalls, FilePath: "a.java", Line: 7},
	}
	store.AddBatch(nodes, edges)
}

func collectOverrideDispatchRows(store graph.Store, scope []string, pageSize int) []graph.OverrideDispatchCall {
	var rows []graph.OverrideDispatchCall
	graph.ScanOverrideDispatchCalls(store, scope, pageSize, func(page []graph.OverrideDispatchCall) bool {
		rows = append(rows, page...)
		return true
	})
	sort.Slice(rows, func(i, j int) bool {
		left, right := rows[i], rows[j]
		if left.CallerLanguage != right.CallerLanguage {
			return left.CallerLanguage < right.CallerLanguage
		}
		if left.From != right.From {
			return left.From < right.From
		}
		return left.Line < right.Line
	})
	return rows
}
