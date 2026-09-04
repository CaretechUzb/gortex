package graph

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Queue accounting for the store-wide resolve mutex (Store.ResolveMutex).
//
// Every edge-mutating pass in the process serialises on it: both resolvers, the
// derived global passes, and the semantic enrichment applies. A pass that finds
// it held is not slow, it is QUEUED — and until this existed that difference
// was invisible from the outside.
//
// Measured, 2026-09-02: a post-track workspace derivation logged "workspace
// derivation starting" at 17:39:40 and its first "cross-repo resolve: pass
// start" at 18:04:25. The 24m45s in between was two semantic enrichment applies
// holding the mutex, and the daemon log said nothing at all for it.
//
// It lives HERE, not with the resolvers, because it describes the mutex rather
// than any one participant: the semantic apply consults it to decide whether
// releasing the mutex mid-pass would actually let somebody in, and
// internal/semantic must not import internal/resolver to ask.
//
// Only passes that would otherwise be starved by a long holder register — the
// resolvers. Enrichment applies deliberately do NOT: they already serialise
// correctly against each other, and if they registered, an apply asking "is
// anyone waiting?" would see its own siblings and yield to them, which is the
// regression this register exists to prevent (measured 2026-09-02: three
// concurrent applies ping-ponging the mutex ran 2.7x slower for identical work
// because each yield dropped their per-pass hot caches).

// ResolveQueueWait is one pass currently blocked on the store-wide resolve
// mutex.
type ResolveQueueWait struct {
	// Pass is the blocked pass's log prefix ("cross-repo resolve", "resolver"),
	// so a status line and a log line name it identically.
	Pass string
	// Since is when it began waiting.
	Since time.Time
}

// resolveQueue tracks in-flight waits so `gortex daemon status` can name one
// WHILE it is happening, and so a mutex holder can tell an empty queue from a
// crowded one. The log line only lands once the wait is over, which is exactly
// too late for a reader asking why the daemon looks idle.
//
// Process-global rather than per-store on purpose: a per-instance register
// would report a fraction of the queue and read as "nothing waiting" from
// whichever instance the caller happened to hold.
var resolveQueue = struct {
	mu      sync.Mutex
	next    atomic.Uint64
	waiting map[uint64]ResolveQueueWait
	// depth mirrors len(waiting) for the hot read path. ResolveQueueBusy is
	// called at every page boundary of every enrichment apply; taking a
	// process-global mutex there would put the applies back in each other's way
	// through a different door.
	depth atomic.Int64
}{waiting: make(map[uint64]ResolveQueueWait)}

// ResolveQueueWaits returns every pass currently blocked on the store-wide
// resolve mutex, oldest wait first. Empty on an idle daemon and on one whose
// passes are running rather than queued — the caller must not read an empty
// slice as "no pass is running".
func ResolveQueueWaits() []ResolveQueueWait {
	resolveQueue.mu.Lock()
	out := make([]ResolveQueueWait, 0, len(resolveQueue.waiting))
	for _, w := range resolveQueue.waiting {
		out = append(out, w)
	}
	resolveQueue.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Since.Equal(out[j].Since) {
			return out[i].Pass < out[j].Pass
		}
		return out[i].Since.Before(out[j].Since)
	})
	return out
}

// ResolveQueueBusy reports whether any pass is currently blocked on the
// store-wide resolve mutex. Lock-free: the answer is a scheduling hint, and a
// caller that misses a wait by nanoseconds simply asks again at its next page.
func ResolveQueueBusy() bool {
	return resolveQueue.depth.Load() > 0
}

// LongestResolveQueueWait returns the age and name of the oldest in-flight wait.
// Zero duration and an empty name when nothing is queued.
func LongestResolveQueueWait(now time.Time) (time.Duration, string) {
	waits := ResolveQueueWaits()
	if len(waits) == 0 {
		return 0, ""
	}
	oldest := waits[0]
	age := now.Sub(oldest.Since)
	if age < 0 {
		age = 0
	}
	return age, oldest.Pass
}

// EnterResolveQueue publishes a wait that is about to begin and returns the
// handle LeaveResolveQueue needs. Callers must pair the two, and only passes
// that a long mutex holder should yield to may register (see the file comment).
func EnterResolveQueue(pass string) uint64 {
	id := resolveQueue.next.Add(1)
	resolveQueue.mu.Lock()
	resolveQueue.waiting[id] = ResolveQueueWait{Pass: pass, Since: time.Now()}
	resolveQueue.mu.Unlock()
	resolveQueue.depth.Add(1)
	return id
}

// LeaveResolveQueue retires the wait EnterResolveQueue published.
func LeaveResolveQueue(id uint64) {
	resolveQueue.mu.Lock()
	_, found := resolveQueue.waiting[id]
	delete(resolveQueue.waiting, id)
	resolveQueue.mu.Unlock()
	// Guarded on found so a double Leave cannot drive the depth negative and
	// leave ResolveQueueBusy reporting "idle" for the rest of the process.
	if found {
		resolveQueue.depth.Add(-1)
	}
}
