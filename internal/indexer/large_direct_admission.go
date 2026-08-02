package indexer

import (
	"context"
	"sync"

	"golang.org/x/sync/semaphore"

	"github.com/zzet/gortex/internal/graph"
)

const (
	// Intrinsically large direct parses remain repository-serial. Their graph
	// batches are bounded, but allowing a nominally lightweight parse to overlap
	// still increased SQLite drain contention and delayed pressure-sized work in
	// the measured cold-start corpus.
	largeDirectAdmissionCapacity      int64 = 1
	largeDirectNativePressureWeight   int64 = 1
	largeDirectLightweightParseWeight int64 = 1
)

// largeDirectAdmissionBudget bounds intrinsically large repository parses that
// stream directly into a durable store. The existing parseAdmissionBudget
// controls live source bytes per file, but it cannot see graph batches, native
// parser high-water, or allocator retention accumulated across a whole large
// repository. Small repositories, in-memory shadows, and bounded streaming
// parses remain concurrent. Scoped incremental reconciliation never enters this
// gate.
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

// largeDirectParseAdmissionWeight admits only full parses whose store and
// intrinsic size can accumulate a repository-scale retained heap. In-memory
// shadows are already covered by shadow admission, and streaming flushes bound
// their graph batches, so both stay parallel. Small repositories also remain
// parallel to preserve cold-start throughput.
func largeDirectParseAdmissionWeight(
	store graph.Store,
	streaming bool,
	files int,
	inputBytes int64,
	nativePressureFiles int,
	nativePressureBytes int64,
) int64 {
	if files <= 0 || streaming {
		return 0
	}
	if _, directStore := store.(graph.BulkLoader); !directStore {
		return 0
	}
	if !largeDirectParseNeedsHeapRelease(true, files, inputBytes) {
		return 0
	}
	if nativePressureFiles >= defaultShadowMaxFileCount ||
		nativePressureBytes >= nativeParsePressureThresholdBytes {
		return largeDirectNativePressureWeight
	}
	return largeDirectLightweightParseWeight
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
