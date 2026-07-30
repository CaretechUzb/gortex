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
	flood       []search.SymbolBundle
	scoped      []search.SymbolBundle
	scopedCalls int
}

func (b *scopedBundleBackend) Add(string, ...string)                    {}
func (b *scopedBundleBackend) Remove(string)                            {}
func (b *scopedBundleBackend) Search(string, int) []search.SearchResult { return nil }
func (b *scopedBundleBackend) Count() int                               { return len(b.flood) }
func (b *scopedBundleBackend) Close()                                   {}

func (b *scopedBundleBackend) SearchSymbolBundles(string, int) []search.SymbolBundle {
	return append([]search.SymbolBundle(nil), b.flood...)
}

func (b *scopedBundleBackend) SearchSymbolBundlesScoped(_ string, _ []string, _ int) []search.SymbolBundle {
	b.scopedCalls++
	return append([]search.SymbolBundle(nil), b.scoped...)
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
