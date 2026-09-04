package resolver

import (
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// The resolvers' half of the store-wide resolve-mutex queue accounting.
//
// The register itself lives in internal/graph, next to the mutex it describes,
// because the semantic enrichment apply consults it to decide whether releasing
// the mutex mid-pass would let anybody in — and internal/semantic must not
// import internal/resolver to ask. What stays here is the resolver-facing
// wrapper: take the mutex, publish the wait for its duration, and say so
// afterwards when it was long enough to explain a quiet stretch in the log.

// resolveQueueLogThreshold is the wait above which a pass announces that it
// spent the time queued rather than working. Deliberately the same 5s the
// tstypes apply gate uses, so the two lines describe one phenomenon at one
// resolution. A var only so a test can shorten it; nothing at runtime writes it.
var resolveQueueLogThreshold = 5 * time.Second

// ResolveQueueWait is one pass currently blocked on the store-wide resolve
// mutex. Aliased rather than redeclared so a caller can pass the register's own
// records straight through.
type ResolveQueueWait = graph.ResolveQueueWait

// ResolveQueueWaits returns every pass currently blocked on the store-wide
// resolve mutex, oldest wait first. Empty on an idle daemon and on one whose
// passes are running rather than queued — the caller must not read an empty
// slice as "no pass is running".
func ResolveQueueWaits() []ResolveQueueWait { return graph.ResolveQueueWaits() }

// LongestResolveQueueWait returns the age and name of the oldest in-flight wait.
// Zero duration and an empty name when nothing is queued.
func LongestResolveQueueWait(now time.Time) (time.Duration, string) {
	return graph.LongestResolveQueueWait(now)
}

// lockResolveQueued takes mu, publishing the wait while it lasts and logging it
// afterwards when it exceeded resolveQueueLogThreshold. pass is the caller's log
// prefix; the emitted message is "<pass>: pass began after queueing".
//
// Registering the wait is not only reporting: a semantic apply holding the mutex
// polls the register at its page boundaries and releases only when somebody is
// actually queued, so a resolver that does not register here waits out the
// holder's whole pass.
//
// Returns the wait so a caller that reports its own timings can subtract work it
// never did.
func lockResolveQueued(mu *sync.Mutex, logger *zap.Logger, pass string) time.Duration {
	if mu == nil {
		return 0
	}
	id := graph.EnterResolveQueue(pass)
	start := time.Now()
	mu.Lock()
	queued := time.Since(start)
	graph.LeaveResolveQueue(id)
	if queued >= resolveQueueLogThreshold && logger != nil {
		logger.Info(pass+": pass began after queueing",
			zap.Duration("queued", queued))
	}
	return queued
}
