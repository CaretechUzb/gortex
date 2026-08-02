package indexer

import (
	"context"
	"os"
	"sync"

	"golang.org/x/sync/semaphore"
)

// parseAdmissionBudget is shared by every per-repository Indexer owned by one
// daemon. Local Indexer budgets still apply; this additional gate prevents the
// parallel warmup/watcher lanes from multiplying that per-repo allowance.
type parseAdmissionBudget struct {
	capacity int64
	mu       sync.Mutex
	used     int64
	waiters  []*parseAdmissionWaiter
	bypasses int
}

type parseAdmissionWaiter struct {
	weight  int64
	ready   chan struct{}
	granted bool
}

// A partial incremental chunk may continue past an already-queued first-file
// waiter so SQLite batching does not collapse immediately under concurrency.
// The allowance is deliberately one less than the 32-file chunk ceiling: once
// spent, every active chunk must flush and release, guaranteeing the FIFO head
// eventually obtains even a capacity-sized reservation.
const maxParseAdmissionBypasses = incrementalBatchFiles - 1

func newParseAdmissionBudget(capacity int64) *parseAdmissionBudget {
	if capacity <= 0 {
		return nil
	}
	return &parseAdmissionBudget{capacity: capacity}
}

func (budget *parseAdmissionBudget) acquire(ctx context.Context, weight int64) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	budget.mu.Lock()
	select {
	case <-ctx.Done():
		budget.mu.Unlock()
		return ctx.Err()
	default:
	}
	if len(budget.waiters) == 0 && budget.used+weight <= budget.capacity {
		budget.used += weight
		budget.mu.Unlock()
		return nil
	}
	waiter := &parseAdmissionWaiter{weight: weight, ready: make(chan struct{})}
	budget.waiters = append(budget.waiters, waiter)
	budget.grantWaitersLocked()
	if waiter.granted {
		budget.mu.Unlock()
		return nil
	}
	budget.mu.Unlock()

	select {
	case <-waiter.ready:
		select {
		case <-ctx.Done():
			budget.release(waiter.weight)
			return ctx.Err()
		default:
			return nil
		}
	case <-ctx.Done():
		budget.mu.Lock()
		if waiter.granted {
			// The reservation won the race with cancellation. Return the
			// caller's cancellation while making the granted capacity
			// immediately available to the next FIFO waiter.
			budget.used -= waiter.weight
		} else {
			for i, queued := range budget.waiters {
				if queued == waiter {
					budget.waiters = append(budget.waiters[:i], budget.waiters[i+1:]...)
					break
				}
			}
		}
		budget.grantWaitersLocked()
		budget.mu.Unlock()
		return ctx.Err()
	}
}

func (budget *parseAdmissionBudget) grantWaitersLocked() {
	for len(budget.waiters) > 0 {
		waiter := budget.waiters[0]
		if budget.used+waiter.weight > budget.capacity {
			return
		}
		budget.waiters[0] = nil
		budget.waiters = budget.waiters[1:]
		budget.used += waiter.weight
		waiter.granted = true
		close(waiter.ready)
		budget.bypasses = 0
	}
	budget.bypasses = 0
}

// tryAcquire admits currently available capacity ahead of a FIFO waiter only
// for a bounded number of chunk continuations. This preserves useful SQLite
// batching without the unbounded starvation of an unfair try-lock or the
// immediate one-file collapse of x/sync/semaphore.TryAcquire.
func (budget *parseAdmissionBudget) tryAcquire(weight int64) bool {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.used+weight > budget.capacity {
		return false
	}
	if len(budget.waiters) > 0 {
		if budget.bypasses >= maxParseAdmissionBypasses {
			return false
		}
		budget.bypasses++
	}
	budget.used += weight
	return true
}

func (budget *parseAdmissionBudget) release(weight int64) {
	if budget == nil || weight <= 0 {
		return
	}
	budget.mu.Lock()
	budget.used -= weight
	budget.grantWaitersLocked()
	budget.mu.Unlock()
}

type parseAdmissionLease struct {
	shared       *parseAdmissionBudget
	sharedWeight int64
	local        *semaphore.Weighted
	localWeight  int64
	once         sync.Once
}

// acquireParseAdmission takes the process-wide lease first and the private
// per-Indexer lease second. Every caller uses that order, so cross-repository
// parsing cannot deadlock. If local admission is cancelled, the already-held
// shared capacity is returned before the error escapes.
func acquireParseAdmission(
	ctx context.Context,
	fileSize int64,
	localBudget int64,
	local *semaphore.Weighted,
	shared *parseAdmissionBudget,
) (*parseAdmissionLease, error) {
	if local == nil && shared == nil {
		return nil, nil
	}
	lease := &parseAdmissionLease{shared: shared, local: local}
	if shared != nil {
		lease.sharedWeight = clampParseWeight(fileSize, shared.capacity)
		if err := shared.acquire(ctx, lease.sharedWeight); err != nil {
			return nil, err
		}
	}
	if local != nil {
		lease.localWeight = clampParseWeight(fileSize, localBudget)
		if err := local.Acquire(ctx, lease.localWeight); err != nil {
			if shared != nil {
				shared.release(lease.sharedWeight)
			}
			return nil, err
		}
	}
	return lease, nil
}

func (lease *parseAdmissionLease) Release() {
	if lease == nil {
		return
	}
	lease.once.Do(func() {
		if lease.local != nil {
			lease.local.Release(lease.localWeight)
		}
		if lease.shared != nil {
			lease.shared.release(lease.sharedWeight)
		}
	})
}

func (idx *Indexer) acquireSharedParsePath(path string) (*parseAdmissionLease, error) {
	shared := idx.parseAdmission.Load()
	if shared == nil {
		return nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		// The caller's authoritative read/stat path reports the filesystem
		// error. No bytes can be materialised, so no admission is needed.
		return nil, nil
	}
	return acquireParseAdmission(context.Background(), info.Size(), 0, nil, shared)
}

// tryAcquireSharedParsePath is the non-blocking counterpart used while an
// incremental chunk already owns prepared results. If the shared budget is
// busy, the caller flushes that partial chunk (releasing its leases) before it
// retries the current file. That rule prevents multiple repositories from
// each retaining a partial chunk while all wait for more shared capacity.
func (idx *Indexer) tryAcquireSharedParsePath(path string) (*parseAdmissionLease, bool) {
	shared := idx.parseAdmission.Load()
	if shared == nil {
		return nil, true
	}
	info, err := os.Stat(path)
	if err != nil {
		// The authoritative read path reports the filesystem error. No bytes
		// can be materialised, so admission must not turn it into "busy".
		return nil, true
	}
	weight := clampParseWeight(info.Size(), shared.capacity)
	if !shared.tryAcquire(weight) {
		return nil, false
	}
	return &parseAdmissionLease{shared: shared, sharedWeight: weight}, true
}

// SetSharedParseMemoryBudget installs one bytes-in-flight budget across every
// repository owned by this MultiIndexer. Call it before starting repository
// mutation workers. The budget remains active for later watcher/Git mutations,
// preventing independent repository lanes from recreating the startup peak.
// A non-positive value disables the shared gate while retaining per-repo caps.
func (mi *MultiIndexer) SetSharedParseMemoryBudget(capacity int64) {
	rawAdmission := newParseAdmissionBudget(capacity)
	nativeAdmission := newParseAdmissionBudget(capacity)
	mi.parseAdmission.Store(rawAdmission)
	mi.nativeParseAdmission.Store(nativeAdmission)

	mi.mu.RLock()
	live := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		live = append(live, idx)
	}
	mi.mu.RUnlock()
	for _, idx := range live {
		idx.parseAdmission.Store(rawAdmission)
		idx.nativeParseAdmission.Store(nativeAdmission)
	}
}
