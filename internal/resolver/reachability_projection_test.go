package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type scriptedImportProjectionStore struct {
	*resolverBatchCountingStore
	projectionCalls int
	projected       map[string][]string
	complete        bool
}

func (s *scriptedImportProjectionStore) ProjectImportAdjacency(filePaths []string) (map[string][]string, bool) {
	s.projectionCalls++
	if !s.complete {
		return nil, false
	}
	out := make(map[string][]string, len(filePaths))
	for _, filePath := range filePaths {
		out[filePath] = append([]string(nil), s.projected[filePath]...)
	}
	return out, true
}

func newReachabilityProjectionFixture(t *testing.T) (*graph.Graph, *scriptedImportProjectionStore, *Resolver, *resolveAllPassIndexes, []*graph.Edge) {
	t.Helper()
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/caller.go", RepoPrefix: "repo", Language: "go"},
		{ID: "dep/one.go", Kind: graph.KindFile, Name: "one.go", FilePath: "dep/one.go", RepoPrefix: "dep", Language: "go"},
		{ID: "dep2/two.go", Kind: graph.KindFile, Name: "two.go", FilePath: "dep2/two.go", RepoPrefix: "dep2", Language: "go"},
	}, nil)
	counting := &resolverBatchCountingStore{Store: g}
	projecting := &scriptedImportProjectionStore{
		resolverBatchCountingStore: counting,
		complete:                   true,
		projected: map[string][]string{
			"repo/caller.go": {"dep/one.go"},
		},
	}
	r := New(projecting)
	indexes := newPendingFrontierPassIndexes(r)
	pending := []*graph.Edge{{
		From: "repo/caller.go::Caller", To: graph.UnresolvedMarker + "Work",
		Kind: graph.EdgeCalls, FilePath: "repo/caller.go",
	}}
	return g, projecting, r, indexes, pending
}

func TestReachabilityProjectionReusesStableCallerAcrossPages(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	indexes.reachabilityFiles["stale/file.go"] = map[string]struct{}{"stale": {}}

	for page := 0; page < 2; page++ {
		sources := indexes.prepare(pending)
		r.warmLookupCacheWithSources(pending, sources)
		if _, ok := r.reachableDirsByFile["repo/caller.go"]["dep"]; !ok {
			t.Fatalf("page %d missing projected dependency directory", page)
		}
		r.clearLookupCache()
		indexes.clearPage()
	}
	if store.projectionCalls != 1 {
		t.Fatalf("projection calls = %d, want 1 across overlapping pages", store.projectionCalls)
	}
	if store.getFileNodesByPathsCalls != 0 || store.getOutEdgesByNodeIDsCalls != 0 {
		t.Fatalf("legacy adjacency reads = file:%d out:%d, want 0/0",
			store.getFileNodesByPathsCalls, store.getOutEdgesByNodeIDsCalls)
	}
	if got := store.nodeIDBatchesContaining("dep/one.go"); got != 1 {
		t.Fatalf("projected target hydration batches = %d, want 1", got)
	}
	if len(indexes.reachabilityFiles) != 2 {
		t.Fatalf("stable pass cache size = %d, want 2", len(indexes.reachabilityFiles))
	}
	// Pass-scoped retention: a stable caller from an earlier page stays cached
	// (its reachability cannot change while the pass holds the resolve mutex),
	// so its reappearance pages later costs no store read.
	if _, retained := indexes.reachabilityFiles["stale/file.go"]; !retained {
		t.Fatal("pass cache dropped a stable caller from an earlier page")
	}
}

func TestReachabilityProjectionCapClearsPathologicalCache(t *testing.T) {
	_, _, r, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	for i := 0; i <= reachabilityStableFileCap; i++ {
		indexes.reachabilityFiles[fmt.Sprintf("bulk/file%06d.go", i)] = map[string]struct{}{"bulk": {}}
	}
	if ok, _ := r.buildReachabilityIndexForPendingCached(pending, nil, indexes.reachabilityFiles); !ok {
		t.Fatal("reachability build failed")
	}
	defer r.clearReachabilityIndex()
	if len(indexes.reachabilityFiles) > 2 {
		t.Fatalf("cap overflow retained %d entries, want wholesale clear", len(indexes.reachabilityFiles))
	}
}

func TestReachabilityProjectionFallsBackForMalformedProvenance(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "repo/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/caller.go"},
		{ID: "dep/target.go", Kind: graph.KindFile, Name: "target.go", FilePath: "dep/target.go"},
	}, []*graph.Edge{{
		From: "repo/caller.go::Caller", To: "dep/target.go", Kind: graph.EdgeImports, FilePath: "",
	}})
	counting := &resolverBatchCountingStore{Store: g}
	store := &scriptedImportProjectionStore{resolverBatchCountingStore: counting, complete: false}
	r := New(store)
	pending := []*graph.Edge{{
		From: "repo/caller.go::Caller", To: graph.UnresolvedMarker + "Work",
		Kind: graph.EdgeCalls, FilePath: "repo/caller.go",
	}}
	if ok, _ := r.buildReachabilityIndexForPendingCached(pending, nil, make(map[string]map[string]struct{})); !ok {
		t.Fatal("reachability was not built through compatibility fallback")
	}
	defer r.clearReachabilityIndex()
	if _, ok := r.reachableDirsByFile["repo/caller.go"]["dep"]; !ok {
		t.Fatal("fallback lost the legacy imported directory")
	}
	if store.projectionCalls != 1 || store.getFileNodesByPathsCalls != 1 || store.getOutEdgesByNodeIDsCalls != 1 {
		t.Fatalf("projection/fallback reads = projection:%d file:%d out:%d, want 1/1/1",
			store.projectionCalls, store.getFileNodesByPathsCalls, store.getOutEdgesByNodeIDsCalls)
	}
}

func TestReachabilityProjectionRefreshInvalidatesPassCache(t *testing.T) {
	_, store, r, indexes, pending := newReachabilityProjectionFixture(t)

	sources := indexes.prepare(pending)
	r.warmLookupCacheWithSources(pending, sources)
	if _, ok := r.reachableDirsByFile["repo/caller.go"]["dep"]; !ok {
		t.Fatal("initial projection missing dep")
	}
	store.projected["repo/caller.go"] = []string{"dep2/two.go"}
	if !indexes.refreshAfterInterleave(pending, true) {
		t.Fatal("forced refresh did not rebuild page indexes")
	}
	if store.projectionCalls != 2 {
		t.Fatalf("projection calls after refresh = %d, want 2", store.projectionCalls)
	}
	got := r.reachableDirsByFile["repo/caller.go"]
	if _, ok := got["dep2"]; !ok {
		t.Fatal("refreshed projection missing dep2")
	}
	if _, stale := got["dep"]; stale {
		t.Fatal("refreshed projection retained stale dep")
	}
	indexes.close()
	if indexes.reachabilityFiles != nil {
		t.Fatal("close retained the pass-local reachability cache")
	}
}

func TestReachabilityProjectionDoesNotCacheUnresolvedImports(t *testing.T) {
	_, store, _, indexes, pending := newReachabilityProjectionFixture(t)
	defer indexes.close()
	store.projected["repo/caller.go"] = []string{graph.UnresolvedMarker + "import::dep"}

	for page := 0; page < 2; page++ {
		indexes.prepare(pending)
		if len(indexes.reachabilityFiles) != 0 {
			t.Fatalf("page %d cached an unresolved import", page)
		}
		indexes.clearPage()
	}
	if store.projectionCalls != 2 {
		t.Fatalf("unresolved projection calls = %d, want 2", store.projectionCalls)
	}
}
