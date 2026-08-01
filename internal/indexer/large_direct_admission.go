package indexer

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"

	"github.com/zzet/gortex/internal/graph"
)

const largeDirectAdmissionCapacity int64 = 1

// largeDirectAdmissionBudget bounds repository-level direct parses. The
// existing parseAdmissionBudget controls live source bytes per file, but it
// cannot see graph batches, native parser high-water, or allocator retention
// accumulated across a whole repository. Intrinsically large direct parses
// therefore take the full one-slot process capacity while ordinary direct and
// shadow parses have zero weight and bypass this gate.
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

// largeDirectParseAdmissionWeight deliberately uses the intrinsic shadow
// ceilings, not operator overrides. Lowering GORTEX_SHADOW_MAX_* or merely
// losing the non-blocking shadow-budget race must not serialize an ordinary
// repository. Streaming shadows are already bounded by their chunk size and
// likewise bypass the direct-store lane.
func largeDirectParseAdmissionWeight(
	store graph.Store,
	streaming bool,
	files int,
	inputBytes int64,
) int64 {
	if streaming || store == nil {
		return 0
	}
	_, directStore := store.(graph.FileMtimeWriter)
	if !largeDirectParseNeedsHeapRelease(directStore, files, inputBytes) {
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
