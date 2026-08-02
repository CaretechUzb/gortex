package query

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// scopedBundleBackend simulates the cross-repo flood at the backend
// boundary: the unscoped bundle call returns only out-of-scope rows
// (the flood is deeper than any head the engine could fetch), while
// the scoped call — narrowing inside the query — reaches the in-scope
// symbol.
type scopedBundleBackend struct {
	flood         []search.SymbolBundle
	scoped        []search.SymbolBundle
	scopedCalls   int
	unscopedCalls int
	searchCalls   int
}

func (b *scopedBundleBackend) Add(string, ...string) {}
func (b *scopedBundleBackend) Remove(string)         {}
func (b *scopedBundleBackend) Search(string, int) []search.SearchResult {
	b.searchCalls++
	return nil
}
func (b *scopedBundleBackend) Count() int { return len(b.flood) }
func (b *scopedBundleBackend) Close()     {}

func (b *scopedBundleBackend) SearchSymbolBundles(string, int) []search.SymbolBundle {
	b.unscopedCalls++
	return append([]search.SymbolBundle(nil), b.flood...)
}

func (b *scopedBundleBackend) SearchSymbolBundlesScoped(_ string, _ []string, _ int) []search.SymbolBundle {
	b.scopedCalls++
	// Preserve nil-vs-empty: a non-nil empty scoped answer is the
	// sentinel contract the engine must honour (append would flatten
	// it back to nil).
	if b.scoped == nil {
		return nil
	}
	out := make([]search.SymbolBundle, len(b.scoped))
	copy(out, b.scoped)
	return out
}

// TestGatherSymbolCandidates_RepoScopeUsesScopedBundles: with a repo
// narrow set and a backend that can scope inside the query, the engine
// must take the scoped path — post-filtering the unscoped head would
// return nothing here.
func TestGatherSymbolCandidates_RepoScopeUsesScopedBundles(t *testing.T) {
	g := graph.New()
	appNode := &graph.Node{ID: "app/w.go::WidgetExtensions", Name: "WidgetExtensions", Kind: graph.KindType, RepoPrefix: "app"}
	g.AddNode(appNode)
	var flood []search.SymbolBundle
	for i := 0; i < 3; i++ {
		n := &graph.Node{ID: "noise/f.go::Extensions", Name: "Extensions", Kind: graph.KindFunction, RepoPrefix: "noise"}
		flood = append(flood, search.SymbolBundle{Node: n, Score: 10})
	}
	backend := &scopedBundleBackend{
		flood:  flood,
		scoped: []search.SymbolBundle{{Node: appNode, Score: 1}},
	}
	engine := NewEngine(g)
	engine.SetSearch(backend)

	opts := QueryOptions{RepoAllow: map[string]bool{"app": true}, SkipInnerRerank: true, SkipVectorChannel: true}
	got := engine.GatherSymbolCandidates("Extensions", 5, opts, nil)
	if len(got) != 1 || got[0].Node.ID != appNode.ID {
		t.Fatalf("scoped gather = %#v, want exactly the in-scope symbol", got)
	}
	if backend.scopedCalls != 1 {
		t.Fatalf("scoped backend path used %d times, want 1", backend.scopedCalls)
	}
}

// TestGatherSymbolCandidates_ScopedZeroDoesNotFloodFetch: a scoped
// query that legitimately answers zero IS the answer — the engine must
// not fall back to the unscoped bundle fetch (the cross-repo flood cost
// the scoped path exists to remove) nor the channel/Search path.
func TestGatherSymbolCandidates_ScopedZeroDoesNotFloodFetch(t *testing.T) {
	g := graph.New()
	noise := &graph.Node{ID: "noise/f.go::Extensions", Name: "Extensions", Kind: graph.KindFunction, RepoPrefix: "noise"}
	g.AddNode(noise)
	backend := &scopedBundleBackend{
		flood:  []search.SymbolBundle{{Node: noise, Score: 10}},
		scoped: []search.SymbolBundle{}, // non-nil empty: scoped answered zero
	}
	engine := NewEngine(g)
	engine.SetSearch(backend)

	opts := QueryOptions{RepoAllow: map[string]bool{"app": true}, SkipInnerRerank: true, SkipVectorChannel: true}
	got := engine.GatherSymbolCandidates("Extensions", 5, opts, nil)
	if len(got) != 0 {
		t.Fatalf("scoped zero answer must yield no candidates, got %#v", got)
	}
	if backend.unscopedCalls != 0 {
		t.Fatalf("unscoped bundle fetch ran %d times after a scoped zero answer", backend.unscopedCalls)
	}
	if backend.searchCalls != 0 {
		t.Fatalf("fallback Search ran %d times after a scoped zero answer", backend.searchCalls)
	}
}

// TestGatherSymbolCandidates_NoScopeKeepsUnscopedBundles: without a
// repo narrow the engine must not pay the scoped path.
func TestGatherSymbolCandidates_NoScopeKeepsUnscopedBundles(t *testing.T) {
	g := graph.New()
	n := &graph.Node{ID: "noise/f.go::Extensions", Name: "Extensions", Kind: graph.KindFunction, RepoPrefix: "noise"}
	g.AddNode(n)
	backend := &scopedBundleBackend{flood: []search.SymbolBundle{{Node: n, Score: 10}}}
	engine := NewEngine(g)
	engine.SetSearch(backend)

	got := engine.GatherSymbolCandidates("Extensions", 5, QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, nil)
	if len(got) != 1 || got[0].Node.ID != n.ID {
		t.Fatalf("unscoped gather = %#v, want the flood row", got)
	}
	if backend.scopedCalls != 0 {
		t.Fatalf("scoped path used without a repo narrow (%d calls)", backend.scopedCalls)
	}
}
