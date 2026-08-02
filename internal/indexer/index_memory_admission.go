package indexer

import (
	"context"
	"os"
	"strconv"
	"sync"

	"golang.org/x/sync/semaphore"
)

const (
	// The process normally retains roughly 1 GiB of SQLite pages, runtime
	// state, and already-persisted graph data during cold startup. Reserving the
	// remaining 3 GiB for active repository parse/shadow phases keeps their
	// combined estimate under an approximately 4 GiB process envelope without
	// forcing ordinary repositories through a single-file lane.
	defaultIndexMemoryAdmissionBytes int64 = 3 << 30

	// shadowAdmissionWeight accounts for raw-source expansion and graph
	// structure. Each active repository also owns worker stacks, parser state,
	// graph batches, and SQLite buffers which do not scale directly with source
	// bytes, so reserve a small fixed amount for that unmodelled phase state.
	indexMemoryAdmissionRepoOverhead int64 = 64 << 20
)

// indexMemoryAdmissionBudget is the process-wide weighted envelope shared by
// full direct parses and in-memory shadows. Direct parses release after their
// parser/batch tail; shadows retain the same lease through their destructive
// drain. This permits small work to overlap while preventing one source-heavy
// direct parse from co-residing with another pressure-sized shadow.
type indexMemoryAdmissionBudget struct {
	capacity int64
	gate     *semaphore.Weighted

	mu         sync.Mutex
	used       int64
	peak       int64
	waiters    int
	admissions uint64
	queued     uint64
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
	admissions uint64
	queued     uint64
}

var processIndexMemoryAdmission = newIndexMemoryAdmissionBudget(indexMemoryAdmissionBytes())

func newIndexMemoryAdmissionBudget(capacity int64) *indexMemoryAdmissionBudget {
	if capacity < 0 {
		capacity = 0
	}
	budget := &indexMemoryAdmissionBudget{capacity: capacity}
	if capacity > 0 {
		budget.gate = semaphore.NewWeighted(capacity)
	}
	return budget
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

func (budget *indexMemoryAdmissionBudget) acquire(
	ctx context.Context,
	weight int64,
) (*indexMemoryAdmissionLease, error) {
	if budget == nil || budget.gate == nil || budget.capacity <= 0 || weight <= 0 {
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

	queued := !budget.gate.TryAcquire(weight)
	if queued {
		budget.mu.Lock()
		budget.waiters++
		budget.queued++
		budget.mu.Unlock()
		if err := budget.gate.Acquire(ctx, weight); err != nil {
			budget.mu.Lock()
			budget.waiters--
			budget.mu.Unlock()
			return nil, err
		}
	}

	budget.mu.Lock()
	if queued {
		budget.waiters--
	}
	budget.used += weight
	budget.admissions++
	if budget.used > budget.peak {
		budget.peak = budget.used
	}
	budget.mu.Unlock()
	return &indexMemoryAdmissionLease{budget: budget, weight: weight}, nil
}

func (lease *indexMemoryAdmissionLease) Release() {
	if lease == nil || lease.budget == nil || lease.weight <= 0 {
		return
	}
	lease.once.Do(func() {
		lease.budget.mu.Lock()
		lease.budget.used -= lease.weight
		if lease.budget.used < 0 {
			lease.budget.used = 0
		}
		lease.budget.mu.Unlock()
		lease.budget.gate.Release(lease.weight)
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
		waiters:    budget.waiters,
		admissions: budget.admissions,
		queued:     budget.queued,
	}
}
