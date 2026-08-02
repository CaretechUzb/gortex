package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type countingFrameworkProjectionStore struct {
	graph.Store
	nodeProjectionCalls   int
	memberProjectionCalls int
}

func (s *countingFrameworkProjectionStore) NodesByKinds(kinds []graph.NodeKind) []*graph.Node {
	s.nodeProjectionCalls++
	return s.Store.(graph.NodesByKindsScanner).NodesByKinds(kinds)
}

func (s *countingFrameworkProjectionStore) MemberMethodsByType() map[string][]graph.MemberMethodInfo {
	s.memberProjectionCalls++
	return s.Store.(graph.MemberMethodsByType).MemberMethodsByType()
}

func newFrameworkProjectionFixture() *countingFrameworkProjectionStore {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "type:A", Kind: graph.KindType, Name: "A", FilePath: "a.go", Language: "go"},
		{ID: "type:B", Kind: graph.KindType, Name: "B", FilePath: "b.go", Language: "go"},
		{ID: "method:A.Run", Kind: graph.KindMethod, Name: "Run", FilePath: "a.go", Language: "go", Meta: map[string]any{"signature": "func Run()"}},
		{ID: "method:A.Stop", Kind: graph.KindMethod, Name: "Stop", FilePath: "a.go", Language: "go"},
		{ID: "func:Start", Kind: graph.KindFunction, Name: "Start", FilePath: "main.go", Language: "go"},
	}, []*graph.Edge{
		{From: "method:A.Run", To: "type:A", Kind: graph.EdgeMemberOf, FilePath: "a.go"},
	})
	return &countingFrameworkProjectionStore{Store: g}
}

func TestFrameworkFullReadCacheReusesOnlyExactOrderedProjections(t *testing.T) {
	store := newFrameworkProjectionFixture()
	cache := newFrameworkFullReadCache()

	read := func(kinds ...graph.NodeKind) {
		t.Helper()
		runLegacyFrameworkSynthWithCache(store, cache, func(g graph.Store) int {
			if got := len(nodesByKindsOrAll(g, kinds...)); got != 3 {
				t.Fatalf("projection rows = %d, want 3", got)
			}
			members := memberMethodInfosByType(g)
			if got := len(members["type:A"]); got != 1 {
				t.Fatalf("member rows = %d, want 1", got)
			}
			return 0
		})
	}

	read(graph.KindMethod, graph.KindFunction)
	read(graph.KindMethod, graph.KindFunction)
	if store.nodeProjectionCalls != 1 || store.memberProjectionCalls != 1 {
		t.Fatalf("backend calls after exact repeat = nodes %d, members %d; want 1, 1",
			store.nodeProjectionCalls, store.memberProjectionCalls)
	}

	// Reversing the kind order is deliberately a distinct key. The cache
	// avoids identical framework scans; it does not claim broad set-overlap
	// reuse or alter the backend's deterministic row ordering.
	read(graph.KindFunction, graph.KindMethod)
	if store.nodeProjectionCalls != 2 {
		t.Fatalf("backend node calls after reversed key = %d, want 2", store.nodeProjectionCalls)
	}
	stats := cache.stats()
	if stats.NodeHits != 1 || stats.NodeMisses != 2 || stats.MemberHits != 2 || stats.MemberMisses != 1 {
		t.Fatalf("unexpected cache stats: %+v", stats)
	}
	if stats.RetainedRows == 0 || stats.RetainedBytes == 0 {
		t.Fatalf("cache retained no bounded projection: %+v", stats)
	}
	if stats.RetainedRows > stats.RowCap || stats.RetainedBytes > stats.ByteCap {
		t.Fatalf("cache exceeded limits: %+v", stats)
	}
}

func TestFrameworkFullReadCacheInvalidatesNodeMutation(t *testing.T) {
	store := newFrameworkProjectionFixture()
	cache := newFrameworkFullReadCache()
	batch := newFrameworkEdgeBatchStoreWithCache(store, cache)

	nodes := batch.NodesByKinds([]graph.NodeKind{graph.KindFunction})
	if len(nodes) != 1 {
		t.Fatalf("initial function rows = %d, want 1", len(nodes))
	}
	updated := nodes[0]
	updated.Meta = map[string]any{"temporal_role": "activity"}
	batch.AddNode(updated)

	reloaded := batch.NodesByKinds([]graph.NodeKind{graph.KindFunction})
	if store.nodeProjectionCalls != 2 {
		t.Fatalf("backend node calls after AddNode = %d, want 2", store.nodeProjectionCalls)
	}
	if len(reloaded) != 1 || reloaded[0].Meta["temporal_role"] != "activity" {
		t.Fatalf("reloaded node did not include persisted mutation: %#v", reloaded)
	}
}

func TestFrameworkFullReadCacheInvalidatesMemberProjectionAtFlush(t *testing.T) {
	store := newFrameworkProjectionFixture()
	cache := newFrameworkFullReadCache()

	runLegacyFrameworkSynthWithCache(store, cache, func(g graph.Store) int {
		if got := len(memberMethodInfosByType(g)["type:A"]); got != 1 {
			t.Fatalf("initial member rows = %d, want 1", got)
		}
		g.AddEdge(&graph.Edge{
			From:     "method:A.Stop",
			To:       "type:A",
			Kind:     graph.EdgeMemberOf,
			FilePath: "a.go",
		})
		return 1
	})

	runLegacyFrameworkSynthWithCache(store, cache, func(g graph.Store) int {
		if got := len(memberMethodInfosByType(g)["type:A"]); got != 2 {
			t.Fatalf("member rows after flush = %d, want 2", got)
		}
		return 0
	})
	if store.memberProjectionCalls != 2 {
		t.Fatalf("backend member calls after member_of flush = %d, want 2", store.memberProjectionCalls)
	}
}

func TestFrameworkFullReadCacheBypassesOversizedProjection(t *testing.T) {
	store := newFrameworkProjectionFixture()
	cache := newFrameworkFullReadCacheWithLimits(1, 1)

	for range 2 {
		batch := newFrameworkEdgeBatchStoreWithCache(store, cache)
		if got := len(batch.NodesByKinds([]graph.NodeKind{graph.KindMethod, graph.KindFunction})); got != 3 {
			t.Fatalf("projection rows = %d, want 3", got)
		}
	}
	stats := cache.stats()
	if store.nodeProjectionCalls != 2 || stats.NodeHits != 0 || stats.NodeMisses != 2 || stats.Bypasses != 2 {
		t.Fatalf("oversized projection was retained: calls=%d stats=%+v", store.nodeProjectionCalls, stats)
	}
	if stats.RetainedRows != 0 || stats.RetainedBytes != 0 {
		t.Fatalf("oversized projection consumed cache budget: %+v", stats)
	}
}

var frameworkProjectionBenchmarkRows int

func BenchmarkFrameworkFullReadCacheExactNodeProjection(b *testing.B) {
	base := graph.New()
	nodes := make([]*graph.Node, 0, 20000)
	for i := range 20000 {
		kind := graph.KindFunction
		if i%2 == 0 {
			kind = graph.KindMethod
		}
		nodes = append(nodes, &graph.Node{
			ID:       fmt.Sprintf("node:%d", i),
			Kind:     kind,
			Name:     fmt.Sprintf("Symbol%d", i),
			FilePath: fmt.Sprintf("pkg/file%d.go", i/20),
			Language: "go",
			Meta:     map[string]any{"signature": "func()"},
		})
	}
	base.AddBatch(nodes, nil)
	store := &countingFrameworkProjectionStore{Store: base}
	kinds := []graph.NodeKind{graph.KindMethod, graph.KindFunction}

	b.Run("uncached", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			batch := newFrameworkEdgeBatchStore(store)
			frameworkProjectionBenchmarkRows = len(batch.NodesByKinds(kinds))
		}
	})
	b.Run("cached", func(b *testing.B) {
		cache := newFrameworkFullReadCache()
		b.ReportAllocs()
		for range b.N {
			batch := newFrameworkEdgeBatchStoreWithCache(store, cache)
			frameworkProjectionBenchmarkRows = len(batch.NodesByKinds(kinds))
		}
	})
}
