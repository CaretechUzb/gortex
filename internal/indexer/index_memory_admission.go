package indexer

import (
	"context"
	"os"
	"strconv"
	"sync"
)

const (
	// The process normally retains SQLite pages, runtime state, and already-
	// persisted graph data during cold startup. Reserve 3 GiB for concurrent
	// repository parse/shadow phases, leaving headroom for that uncharged state.
	// This is an admission estimate, not a hard RSS limit.
	defaultIndexMemoryAdmissionBytes int64 = 3 << 30

	// shadowAdmissionWeight accounts for raw-source expansion and graph
	// structure. Each active repository also owns worker stacks, parser state,
	// graph batches, and SQLite buffers which do not scale directly with source
	// bytes, so reserve a small fixed amount for that unmodelled phase state.
	indexMemoryAdmissionRepoOverhead int64 = 64 << 20

	// A pressure-sized head waiter may be temporarily bypassed by fitting small
	// repositories, but only this many times. Once spent, new work queues until
	// the head can enter, bounding starvation while preserving otherwise-idle
	// capacity during a long direct parse.
	maxIndexMemoryAdmissionBypasses = 8
)

// indexMemoryAdmissionBudget is the process-wide weighted envelope shared by
// full direct parses and in-memory shadows. Direct parses release after their
// parser/batch tail; shadows retain the same lease through their destructive
// drain. This permits small work to overlap while preventing one source-heavy
// direct parse from co-residing with another pressure-sized shadow.
type indexMemoryAdmissionBudget struct {
	capacity int64

	mu         sync.Mutex
	used       int64
	peak       int64
	waiters    []*indexMemoryAdmissionWaiter
	bypasses   int
	admissions uint64
	queued     uint64
}

type indexMemoryAdmissionWaiter struct {
	weight  int64
	ready   chan struct{}
	granted bool
}

type indexMemoryAdmissionLease struct {
	budget *indexMemoryAdmissionBudget
	weight int64
	once   sync.Once
}

type indexMemoryAdmissionStats struct {
	capacity   int64
	used       int64
	peak       int64
	waiters    int
	bypasses   int
	admissions uint64
	queued     uint64
}

var processIndexMemoryAdmission = newIndexMemoryAdmissionBudget(indexMemoryAdmissionBytes())

func newIndexMemoryAdmissionBudget(capacity int64) *indexMemoryAdmissionBudget {
	if capacity < 0 {
		capacity = 0
	}
	return &indexMemoryAdmissionBudget{capacity: capacity}
}

// indexMemoryAdmissionBytes returns the process-wide active indexing budget.
// Zero explicitly disables the envelope. Invalid or negative overrides retain
// the balanced default.
func indexMemoryAdmissionBytes() int64 {
	if raw := os.Getenv("GORTEX_INDEX_MEMORY_BUDGET_BYTES"); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err == nil && value >= 0 {
			return value
		}
	}
	return defaultIndexMemoryAdmissionBytes
}

func indexMemoryAdmissionWeight(fileCount int, inputBytes int64) int64 {
	weight := shadowAdmissionWeight(fileCount, inputBytes)
	const maxInt64 = int64(^uint64(0) >> 1)
	if weight > maxInt64-indexMemoryAdmissionRepoOverhead {
		return maxInt64
	}
	return weight + indexMemoryAdmissionRepoOverhead
}

// saturatingAddByteCount accumulates non-negative byte counts without allowing
// an extreme corpus to wrap an admission estimate back to a small value.
func saturatingAddByteCount(total, next int64) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	if total < 0 {
		total = 0
	}
	if next <= 0 {
		return total
	}
	if total > maxInt64-next {
		return maxInt64
	}
	return total + next
}

func (budget *indexMemoryAdmissionBudget) acquire(
	ctx context.Context,
	weight int64,
) (*indexMemoryAdmissionLease, error) {
	if budget == nil || budget.capacity <= 0 || weight <= 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	if weight > budget.capacity {
		weight = budget.capacity
	}

	budget.mu.Lock()
	select {
	case <-ctx.Done():
		budget.mu.Unlock()
		return nil, ctx.Err()
	default:
	}
	if len(budget.waiters) == 0 && weight <= budget.capacity-budget.used {
		budget.used += weight
		budget.admissions++
		if budget.used > budget.peak {
			budget.peak = budget.used
		}
		budget.mu.Unlock()
		select {
		case <-ctx.Done():
			budget.release(weight)
			return nil, ctx.Err()
		default:
			return &indexMemoryAdmissionLease{budget: budget, weight: weight}, nil
		}
	}

	waiter := &indexMemoryAdmissionWaiter{weight: weight, ready: make(chan struct{})}
	budget.waiters = append(budget.waiters, waiter)
	budget.queued++
	budget.grantWaitersLocked()
	budget.mu.Unlock()

	select {
	case <-waiter.ready:
		select {
		case <-ctx.Done():
			budget.release(weight)
			return nil, ctx.Err()
		default:
			return &indexMemoryAdmissionLease{budget: budget, weight: weight}, nil
		}
	case <-ctx.Done():
		budget.mu.Lock()
		if waiter.granted {
			budget.used -= weight
		} else {
			for i, queued := range budget.waiters {
				if queued != waiter {
					continue
				}
				budget.removeWaiterLocked(i)
				if i == 0 {
					budget.bypasses = 0
				}
				break
			}
		}
		budget.grantWaitersLocked()
		budget.mu.Unlock()
		return nil, ctx.Err()
	}
}

// grantWaitersLocked admits the FIFO head when it fits. While it does not fit,
// the oldest later waiter that does fit may bypass it up to a fixed bound.
// Caller holds budget.mu.
func (budget *indexMemoryAdmissionBudget) grantWaitersLocked() {
	for len(budget.waiters) > 0 {
		available := budget.capacity - budget.used
		if budget.waiters[0].weight <= available {
			budget.grantWaiterLocked(0)
			budget.bypasses = 0
			continue
		}
		if budget.bypasses >= maxIndexMemoryAdmissionBypasses {
			return
		}
		fit := -1
		for i := 1; i < len(budget.waiters); i++ {
			if budget.waiters[i].weight <= available {
				fit = i
				break
			}
		}
		if fit < 0 {
			return
		}
		budget.grantWaiterLocked(fit)
		budget.bypasses++
	}
	budget.bypasses = 0
}

// grantWaiterLocked removes and wakes one fitting waiter. Caller holds
// budget.mu and has already checked capacity.
func (budget *indexMemoryAdmissionBudget) grantWaiterLocked(index int) {
	waiter := budget.waiters[index]
	budget.removeWaiterLocked(index)
	budget.used += waiter.weight
	budget.admissions++
	if budget.used > budget.peak {
		budget.peak = budget.used
	}
	waiter.granted = true
	close(waiter.ready)
}

func (budget *indexMemoryAdmissionBudget) removeWaiterLocked(index int) {
	copy(budget.waiters[index:], budget.waiters[index+1:])
	last := len(budget.waiters) - 1
	budget.waiters[last] = nil
	budget.waiters = budget.waiters[:last]
}

func (budget *indexMemoryAdmissionBudget) release(weight int64) {
	if budget == nil || weight <= 0 {
		return
	}
	budget.mu.Lock()
	budget.used -= weight
	if budget.used < 0 {
		budget.used = 0
	}
	budget.grantWaitersLocked()
	budget.mu.Unlock()
}

func (lease *indexMemoryAdmissionLease) Release() {
	if lease == nil || lease.budget == nil || lease.weight <= 0 {
		return
	}
	lease.once.Do(func() {
		lease.budget.release(lease.weight)
	})
}

func (budget *indexMemoryAdmissionBudget) snapshot() indexMemoryAdmissionStats {
	if budget == nil {
		return indexMemoryAdmissionStats{}
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return indexMemoryAdmissionStats{
		capacity:   budget.capacity,
		used:       budget.used,
		peak:       budget.peak,
		waiters:    len(budget.waiters),
		bypasses:   budget.bypasses,
		admissions: budget.admissions,
		queued:     budget.queued,
	}
}
