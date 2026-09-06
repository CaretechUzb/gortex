package tstypes

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func applyHotTestGraph() *graph.Graph {
	g := graph.New()
	widget := &graph.Node{
		ID:         "repo/ui.ts::Widget",
		Kind:       graph.KindType,
		Name:       "Widget",
		FilePath:   "repo/ui.ts",
		Language:   "typescript",
		RepoPrefix: "repo",
	}
	base := &graph.Node{
		ID:         "repo/base.ts::Base",
		Kind:       graph.KindType,
		Name:       "Base",
		FilePath:   "repo/base.ts",
		Language:   "typescript",
		RepoPrefix: "repo",
	}
	g.AddBatch([]*graph.Node{widget, base}, []*graph.Edge{{
		From: widget.ID,
		To:   base.ID,
		Kind: graph.EdgeExtends,
	}})
	return g
}

func TestApplyHotCacheRotationBoundsBytes(t *testing.T) {
	budget := int64(64 << 10)
	c := newApplyHotCache(budget)
	for i := 0; i < 4096; i++ {
		c.putNode(&graph.Node{
			ID:         fmt.Sprintf("repo/f.ts::T%05d", i),
			Name:       fmt.Sprintf("T%05d", i),
			FilePath:   "repo/f.ts",
			Language:   "typescript",
			RepoPrefix: "repo",
		})
	}
	retained := c.cur.bytes + c.prev.bytes
	if retained > 2*budget {
		t.Fatalf("retained %d bytes exceeds 2x budget %d", retained, budget)
	}
	if _, ok := c.getNode("repo/f.ts::T04095"); !ok {
		t.Fatal("most recent entry evicted by rotation")
	}
}

func TestApplyHotCacheSharesLookupsAcrossAppliers(t *testing.T) {
	store := &lookupCountingStore{Store: applyHotTestGraph()}
	hot := newApplyHotCache(1 << 20)
	wanted := map[string]nameRole{"Widget": roleType, "DefinitelyMissing": roleType}

	first := newApplier(store, TypeScriptSpec(), "test-types").withHotCache(hot)
	first.preloadNames("repo", wanted)
	first.nodes([]string{"repo/base.ts::Base"})
	first.loadAdjacency([]string{"repo/ui.ts::Widget"})

	nameCalls := store.repoNameBatchCalls
	nodeCalls := store.nodesByIDCalls
	outCalls := store.outEdgeBatchCalls
	inCalls := store.inEdgeBatchCalls
	if nameCalls == 0 || nodeCalls == 0 || outCalls == 0 || inCalls == 0 {
		t.Fatalf("first applier should hit the store (names=%d nodes=%d out=%d in=%d)",
			nameCalls, nodeCalls, outCalls, inCalls)
	}

	// A later page's fresh applier asks the same questions; every answer —
	// including the negative name group — must come from the shared cache.
	second := newApplier(store, TypeScriptSpec(), "test-types").withHotCache(hot)
	second.preloadNames("repo", wanted)
	second.nodes([]string{"repo/base.ts::Base"})
	second.loadAdjacency([]string{"repo/ui.ts::Widget"})

	if got := store.repoNameBatchCalls; got != nameCalls {
		t.Fatalf("second applier re-queried names: %d -> %d", nameCalls, got)
	}
	if got := store.nodesByIDCalls; got != nodeCalls {
		t.Fatalf("second applier re-queried nodes: %d -> %d", nodeCalls, got)
	}
	if got := store.outEdgeBatchCalls; got != outCalls {
		t.Fatalf("second applier re-queried out adjacency: %d -> %d", outCalls, got)
	}
	if got := store.inEdgeBatchCalls; got != inCalls {
		t.Fatalf("second applier re-queried in adjacency: %d -> %d", inCalls, got)
	}
	if second.node("repo/ui.ts::Widget") == nil {
		t.Fatal("cached name group did not hydrate the second applier")
	}
	// Shared pointers, not copies: same-pass Meta stamps must be visible to
	// later pages through the cache.
	if first.node("repo/base.ts::Base") != second.node("repo/base.ts::Base") {
		t.Fatal("cache must share node pointers so same-pass stamps stay visible")
	}
}

func TestApplyHotCacheFlushAdjacencyForcesRefetch(t *testing.T) {
	store := &lookupCountingStore{Store: applyHotTestGraph()}
	hot := newApplyHotCache(1 << 20)

	first := newApplier(store, TypeScriptSpec(), "test-types").withHotCache(hot)
	first.preloadNames("repo", map[string]nameRole{"Widget": roleType})
	first.loadAdjacency([]string{"repo/ui.ts::Widget"})
	nameCalls := store.repoNameBatchCalls
	outCalls := store.outEdgeBatchCalls

	// Phase boundary: synthesized inheritance edges must become observable,
	// so adjacency drops; nodes and name groups survive.
	hot.flushAdjacency()

	second := newApplier(store, TypeScriptSpec(), "test-types").withHotCache(hot)
	second.preloadNames("repo", map[string]nameRole{"Widget": roleType})
	second.loadAdjacency([]string{"repo/ui.ts::Widget"})

	if got := store.repoNameBatchCalls; got != nameCalls {
		t.Fatalf("flushAdjacency must not drop name groups: %d -> %d", nameCalls, got)
	}
	if got := store.outEdgeBatchCalls; got <= outCalls {
		t.Fatalf("adjacency not re-fetched after flush: %d -> %d", outCalls, got)
	}
}

func TestApplyHotCacheDisabledIsNilSafe(t *testing.T) {
	if c := newApplyHotCache(0); c != nil {
		t.Fatal("zero budget must disable the cache")
	}
	var c *applyHotCache
	c.putNode(&graph.Node{ID: "x"})
	c.putNames(typeCandidateKey{repoPrefix: "r", name: "n"}, nil)
	c.putOut("x", nil)
	c.putIn("x", nil)
	c.flushAdjacency()
	if _, ok := c.getNode("x"); ok {
		t.Fatal("nil cache reported a hit")
	}
	if _, ok := c.getNames(typeCandidateKey{repoPrefix: "r", name: "n"}); ok {
		t.Fatal("nil cache reported a name hit")
	}
	if _, ok := c.getOut("x"); ok {
		t.Fatal("nil cache reported an adjacency hit")
	}

	// An applier without a cache must behave exactly as before.
	store := &lookupCountingStore{Store: applyHotTestGraph()}
	ap := newApplier(store, TypeScriptSpec(), "test-types")
	ap.preloadNames("repo", map[string]nameRole{"Widget": roleType})
	ap.loadAdjacency([]string{"repo/ui.ts::Widget"})
	if ap.node("repo/ui.ts::Widget") == nil {
		t.Fatal("cacheless applier failed to hydrate")
	}
}

func TestApplyHotCacheCachesNegativeNameGroups(t *testing.T) {
	c := newApplyHotCache(1 << 20)
	key := typeCandidateKey{repoPrefix: "repo", name: "NothingHere"}
	if _, ok := c.getNames(key); ok {
		t.Fatal("unexpected hit before put")
	}
	c.putNames(key, nil)
	group, ok := c.getNames(key)
	if !ok {
		t.Fatal("cached negative name group not returned")
	}
	if len(group) != 0 {
		t.Fatalf("cached negative returned %d nodes", len(group))
	}
}
