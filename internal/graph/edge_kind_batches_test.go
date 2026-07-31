package graph

import (
	"fmt"
	"reflect"
	"testing"
)

func TestScanEdgesByKindsBatchedFallbackBoundsAndDeduplicates(t *testing.T) {
	g := New()
	var want []string
	for i := 0; i < 11; i++ {
		kind := EdgeCalls
		if i%2 == 0 {
			kind = EdgeReads
		}
		edge := &Edge{
			From: fmt.Sprintf("from-%02d", i), To: fmt.Sprintf("to-%02d", i),
			Kind: kind, FilePath: "repo/file.go", Line: i + 1,
		}
		g.AddEdge(edge)
		want = append(want, edge.From)
	}

	var got []string
	var batchSizes []int
	ScanEdgesByKindsBatched(g, []EdgeKind{EdgeReads, "", EdgeCalls, EdgeReads}, 3, func(batch []*Edge) bool {
		batchSizes = append(batchSizes, len(batch))
		for _, edge := range batch {
			got = append(got, edge.From)
		}
		return true
	})
	if len(got) != len(want) {
		t.Fatalf("visited %d edges, want %d: %v", len(got), len(want), got)
	}
	seen := make(map[string]int, len(got))
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		if seen[id] != 1 {
			t.Fatalf("edge %q visited %d times", id, seen[id])
		}
	}
	for _, size := range batchSizes {
		if size < 1 || size > 3 {
			t.Fatalf("batch size = %d, want 1..3", size)
		}
	}
}

func TestScanEdgesByKindsBatchedFallbackHonorsEarlyStopAndMinimumSize(t *testing.T) {
	g := New()
	for i := 0; i < 3; i++ {
		g.AddEdge(&Edge{
			From: fmt.Sprintf("from-%d", i), To: fmt.Sprintf("to-%d", i),
			Kind: EdgeReads, FilePath: "repo/file.go", Line: i + 1,
		})
	}
	calls := 0
	ScanEdgesByKindsBatched(g, []EdgeKind{EdgeReads}, 0, func(batch []*Edge) bool {
		calls++
		if len(batch) != 1 {
			t.Fatalf("batch size = %d, want 1", len(batch))
		}
		return false
	})
	if calls != 1 {
		t.Fatalf("callback calls = %d, want 1", calls)
	}

	ScanEdgesByKindsBatched(g, nil, 2, func([]*Edge) bool {
		t.Fatal("empty kind set invoked callback")
		return false
	})
}

type recordingEdgeBatchStore struct {
	Store
	kinds     []EdgeKind
	batchSize int
	called    bool
}

func (s *recordingEdgeBatchStore) ScanEdgesByKindsBatched(
	kinds []EdgeKind,
	batchSize int,
	yield func([]*Edge) bool,
) {
	s.called = true
	s.kinds = append([]EdgeKind(nil), kinds...)
	s.batchSize = batchSize
	yield([]*Edge{{From: "from", To: "to", Kind: EdgeReads}})
}

func TestScanEdgesByKindsBatchedPrefersCapability(t *testing.T) {
	store := &recordingEdgeBatchStore{}
	calls := 0
	ScanEdgesByKindsBatched(store, []EdgeKind{EdgeReads, "", EdgeReads}, 7, func(batch []*Edge) bool {
		calls++
		return len(batch) == 1
	})
	if !store.called || calls != 1 {
		t.Fatalf("capability called=%v callbacks=%d", store.called, calls)
	}
	if !reflect.DeepEqual(store.kinds, []EdgeKind{EdgeReads}) || store.batchSize != 7 {
		t.Fatalf("capability args kinds=%v batchSize=%d", store.kinds, store.batchSize)
	}
}
