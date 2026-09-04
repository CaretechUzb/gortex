package tstypes

import (
	"runtime"
	"sync"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// mutationRevisioner is the store's optional monotonic invalidation token for
// node and edge state. The production SQLite store and the in-memory Graph both
// implement it; test doubles generally do not, and for them yieldResolveMutex
// reports "assume the graph moved" so a released lock can never leave a stale
// cache standing.
type mutationRevisioner interface {
	MutationRevision() uint64
}

func storeMutationRevision(g graph.Store) (uint64, bool) {
	revisioned, ok := g.(mutationRevisioner)
	if !ok {
		return 0, false
	}
	return revisioned.MutationRevision(), true
}

// yieldResolveMutex releases the graph-wide resolve mutex when — and only when
// — a resolver pass is queued behind it, gives that pass a turn, and retakes
// the mutex. It reports whether the graph changed in between, and how long the
// re-acquire cost, so the caller can refund the wait to its own budget.
//
// Modelled on CrossRepoResolver.ResolveAllContext's super-chunk yield, which
// has done exactly this since the resolver's own passes grew long enough to
// starve their neighbours. Go's mutex hands off to a waiter that has been
// blocked more than 1ms, so a queued pass really does get in here rather than
// losing to the yielding goroutine's own re-acquire.
//
// The graph.ResolveQueueBusy gate is the whole difference between helping and
// harming, and it is not an optimisation. Yielding unconditionally starves
// nobody but costs everybody: the release lets a SIBLING enrichment apply in,
// its writes move the store's mutation revision, and the revision check below
// then drops this pass's hot cache — the per-pass read-through cache that
// exists because page hydration was 48-62% of process CPU. Measured 2026-09-02
// on one repo's typescript apply, identical work both times: alone, 89.8% node
// / 76.5% adjacency hit rate in 11s; with two sibling applies interleaving,
// 61.0% / 54.7% in 30s. Python's apply, 2.7x slower the same way, overran its
// budget and landed a partial that erased the provider's readiness row. Applies
// already serialise correctly against each other by simply holding the mutex;
// only a resolver — which can otherwise wait out a 13-minute apply, and once
// waited 24m45s — is worth the cache.
//
// A store that cannot report a revision is treated as having moved. That is the
// safe direction: the only cost is a dropped cache, and the alternative is
// serving a page from entries an interleaving writer invalidated.
func yieldResolveMutex(g graph.Store, mu *sync.Mutex) (graphMutated bool, waited time.Duration) {
	if mu == nil || !graph.ResolveQueueBusy() {
		return false, 0
	}
	before, known := storeMutationRevision(g)
	mu.Unlock()
	runtime.Gosched()
	start := time.Now()
	mu.Lock()
	waited = time.Since(start)
	if !known {
		return true, waited
	}
	after, _ := storeMutationRevision(g)
	return after != before, waited
}
