package indexer

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"

	"github.com/zzet/gortex/internal/graph"
)

const largeDirectAdmissionCapacity int64 = 1

// largeDirectAdmissionBudget bounds complete repository parse phases. The
// existing parseAdmissionBudget controls live source bytes per file, but it
// cannot see graph batches, native parser high-water, or allocator retention
// accumulated across a whole repository. Every full-tree parse therefore takes
// the one process-wide slot; its internal parser workers still run concurrently.
// Scoped incremental reconciliation never enters this gate.
type largeDirectAdmissionBudget struct {
	capacity int64
	gate     *semaphore.Weighted

	mu      sync.Mutex
	used    int64
	peak    int64
	waiters int
}

type largeDirectAdmissionLease struct {
	budget *largeDirectAdmissionBudget
	weight int64
	once   sync.Once
}

type largeDirectAdmissionStats struct {
	capacity int64
	used     int64
	peak     int64
	waiters  int
}

var processLargeDirectAdmission = newLargeDirectAdmissionBudget(largeDirectAdmissionCapacity)

func newLargeDirectAdmissionBudget(capacity int64) *largeDirectAdmissionBudget {
	budget := &largeDirectAdmissionBudget{capacity: capacity}
	if capacity > 0 {
		budget.gate = semaphore.NewWeighted(capacity)
	}
	return budget
}

// largeDirectParseAdmissionWeight admits every non-empty full-repository parse.
// Store shape, streaming mode, and intrinsic size no longer bypass this gate:
// even an individually ordinary shadow retains graph and native-parser state
// that amplifies when several repositories overlap. Per-file parser admission
// remains independent inside the admitted repository.
func largeDirectParseAdmissionWeight(
	_ graph.Store,
	_ bool,
	files int,
	_ int64,
) int64 {
	if files <= 0 {
		return 0
	}
	return largeDirectAdmissionCapacity
}

func (budget *largeDirectAdmissionBudget) acquire(
	ctx context.Context,
	weight int64,
) (*largeDirectAdmissionLease, error) {
	if budget == nil || budget.gate == nil || budget.capacity <= 0 || weight <= 0 {
		return nil, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if weight > budget.capacity {
		weight = budget.capacity
	}

	budget.mu.Lock()
	budget.waiters++
	budget.mu.Unlock()
	err := budget.gate.Acquire(ctx, weight)
	budget.mu.Lock()
	budget.waiters--
	if err == nil {
		budget.used += weight
		if budget.used > budget.peak {
			budget.peak = budget.used
		}
	}
	budget.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &largeDirectAdmissionLease{budget: budget, weight: weight}, nil
}

func (lease *largeDirectAdmissionLease) Release() {
	if lease == nil || lease.budget == nil || lease.weight <= 0 {
		return
	}
	lease.once.Do(func() {
		lease.budget.mu.Lock()
		lease.budget.used -= lease.weight
		lease.budget.mu.Unlock()
		lease.budget.gate.Release(lease.weight)
	})
}

func (budget *largeDirectAdmissionBudget) snapshot() largeDirectAdmissionStats {
	if budget == nil {
		return largeDirectAdmissionStats{}
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	return largeDirectAdmissionStats{
		capacity: budget.capacity,
		used:     budget.used,
		peak:     budget.peak,
		waiters:  budget.waiters,
	}
}
