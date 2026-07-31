package query

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// warmStoreBackend models a backend over a pre-populated disk FTS
// right after a daemon restart: the Add/Remove delta counter is zero
// (nothing indexed THIS process) but the disk corpus is fully loaded.
type warmStoreBackend struct {
	hits []search.SearchResult
	docs int
}

func (b *warmStoreBackend) Add(string, ...string) {}
func (b *warmStoreBackend) Remove(string)         {}
func (b *warmStoreBackend) Count() int            { return 0 }
func (b *warmStoreBackend) Close()                {}
func (b *warmStoreBackend) DocCount() (int, bool) { return b.docs, true }
func (b *warmStoreBackend) Search(string, int) []search.SearchResult {
	return append([]search.SearchResult(nil), b.hits...)
}

// TestGatherSymbolCandidates_WarmStoreUsesBackend: a zero delta
// counter must not divert the engine to the substring fallback when
// the backend's authoritative DocCount proves the corpus exists —
// otherwise every daemon restart over an existing store silently
// degrades search until the first file edit bumps the counter.
func TestGatherSymbolCandidates_WarmStoreUsesBackend(t *testing.T) {
	g := graph.New()
	n := &graph.Node{ID: "app/w.go::WidgetExtensions", Name: "WidgetExtensions", Kind: graph.KindType, RepoPrefix: "app"}
	g.AddNode(n)
	backend := &warmStoreBackend{hits: []search.SearchResult{{ID: n.ID, Score: 5}}, docs: 12345}
	engine := NewEngine(g)
	engine.SetSearch(backend)

	// A two-word concept query: the substring fallback cannot match it
	// against "WidgetExtensions", so a result can only come from the
	// backend — the discriminator between the two paths.
	got := engine.GatherSymbolCandidates("widget extensions", 5, QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, nil)
	if len(got) != 1 || got[0].Node.ID != n.ID {
		t.Fatalf("warm-store backend was bypassed (substring fallback?); got %#v", got)
	}
	if got[0].TextRank != 0 {
		t.Fatalf("backend hit must carry its text rank; got %#v", got[0])
	}
}

// TestGatherSymbolCandidates_EmptyBackendStillFallsBack: no delta
// count AND no doc count keeps the substring fallback for genuinely
// empty in-process backends.
func TestGatherSymbolCandidates_EmptyBackendStillFallsBack(t *testing.T) {
	g := graph.New()
	n := &graph.Node{ID: "app/w.go::WidgetExtensions", Name: "WidgetExtensions", Kind: graph.KindType, RepoPrefix: "app"}
	g.AddNode(n)
	engine := NewEngine(g)
	engine.SetSearch(search.NewBM25()) // empty: Count 0, no corpus

	got := engine.GatherSymbolCandidates("WidgetExtensions", 5, QueryOptions{SkipInnerRerank: true, SkipVectorChannel: true}, nil)
	if len(got) != 1 || got[0].Node.ID != n.ID {
		t.Fatalf("substring fallback must still rescue empty backends; got %#v", got)
	}
}
