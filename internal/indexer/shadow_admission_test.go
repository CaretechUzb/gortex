package indexer

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

func TestShadowAdmissionQueuesUntilRelease(t *testing.T) {
	budget := newShadowAdmissionBudget(1024, 1)
	first, err := budget.acquire(context.Background(), 512)
	if err != nil || first == nil {
		t.Fatalf("first acquire = (%v, %v), want lease, nil", first, err)
	}

	type outcome struct {
		lease *shadowAdmissionLease
		err   error
	}
	result := make(chan outcome, 1)
	go func() {
		lease, acquireErr := budget.acquire(context.Background(), 512)
		result <- outcome{lease: lease, err: acquireErr}
	}()
	waitForShadowWaiters(t, budget, 1)
	select {
	case got := <-result:
		t.Fatalf("queued acquire completed before release: %+v", got)
	default:
	}

	first.Release()
	select {
	case got := <-result:
		if got.err != nil || got.lease == nil {
			t.Fatalf("queued acquire = (%v, %v), want lease, nil", got.lease, got.err)
		}
		got.lease.Release()
	case <-time.After(time.Second):
		t.Fatal("queued acquire did not resume after release")
	}
	assertShadowAdmissionIdle(t, budget)
}

func TestShadowAdmissionHardConcurrentCap(t *testing.T) {
	budget := newShadowAdmissionBudget(4096, 2)
	first, err := budget.acquire(context.Background(), 256)
	if err != nil || first == nil {
		t.Fatalf("first acquire = (%v, %v), want lease, nil", first, err)
	}
	second, err := budget.acquire(context.Background(), 256)
	if err != nil || second == nil {
		t.Fatalf("second acquire = (%v, %v), want lease, nil", second, err)
	}

	thirdResult := make(chan *shadowAdmissionLease, 1)
	go func() {
		lease, _ := budget.acquire(context.Background(), 256)
		thirdResult <- lease
	}()
	waitForShadowWaiters(t, budget, 1)
	stats := budget.snapshot()
	if stats.active != 2 || stats.peakActive != 2 {
		t.Fatalf("active/peak = %d/%d, want 2/2", stats.active, stats.peakActive)
	}

	first.Release()
	third := <-thirdResult
	if third == nil {
		t.Fatal("third acquire returned nil after a slot was released")
	}
	second.Release()
	third.Release()
	assertShadowAdmissionIdle(t, budget)
}

func TestShadowAdmissionWeightedCapacity(t *testing.T) {
	budget := newShadowAdmissionBudget(1000, 2)
	first, err := budget.acquire(context.Background(), 700)
	if err != nil || first == nil {
		t.Fatalf("first acquire = (%v, %v), want lease, nil", first, err)
	}

	result := make(chan *shadowAdmissionLease, 1)
	go func() {
		lease, _ := budget.acquire(context.Background(), 400)
		result <- lease
	}()
	waitForShadowWaiters(t, budget, 1)
	first.Release()
	second := <-result
	if second == nil {
		t.Fatal("weighted waiter returned nil after capacity was released")
	}
	second.Release()
	assertShadowAdmissionIdle(t, budget)
}

func TestShadowAdmissionCancellationRemovesWaiter(t *testing.T) {
	budget := newShadowAdmissionBudget(1024, 1)
	first, err := budget.acquire(context.Background(), 512)
	if err != nil || first == nil {
		t.Fatalf("first acquire = (%v, %v), want lease, nil", first, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, acquireErr := budget.acquire(ctx, 512)
		result <- acquireErr
	}()
	waitForShadowWaiters(t, budget, 1)
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled acquire error = %v, want context.Canceled", err)
	}
	stats := budget.snapshot()
	if stats.waiters != 0 || stats.active != 1 || stats.used != 512 {
		t.Fatalf("stats after cancellation = %+v, want only first lease charged", stats)
	}
	first.Release()
	assertShadowAdmissionIdle(t, budget)
}

func TestShadowAdmissionGrantCancellationRaceDoesNotLeak(t *testing.T) {
	for range 200 {
		budget := newShadowAdmissionBudget(1024, 1)
		first, err := budget.acquire(context.Background(), 512)
		if err != nil || first == nil {
			t.Fatalf("first acquire = (%v, %v), want lease, nil", first, err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		result := make(chan error, 1)
		go func() {
			lease, acquireErr := budget.acquire(ctx, 512)
			if lease != nil {
				lease.Release()
			}
			result <- acquireErr
		}()
		waitForShadowWaiters(t, budget, 1)
		go first.Release()
		cancel()
		if err := <-result; err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("racing acquire error = %v, want nil or context.Canceled", err)
		}
		waitForShadowIdle(t, budget)
	}
}

func TestShadowAdmissionDisabledAndOversizedFallBack(t *testing.T) {
	for _, test := range []struct {
		name          string
		capacity      int64
		maxConcurrent int
		weight        int64
	}{
		{name: "zero capacity", capacity: 0, maxConcurrent: 1, weight: 1},
		{name: "zero slots", capacity: 1024, maxConcurrent: 0, weight: 1},
		{name: "oversized", capacity: 1024, maxConcurrent: 1, weight: 1025},
	} {
		t.Run(test.name, func(t *testing.T) {
			budget := newShadowAdmissionBudget(test.capacity, test.maxConcurrent)
			lease, err := budget.acquire(context.Background(), test.weight)
			if err != nil || lease != nil {
				t.Fatalf("fallback acquire = (%v, %v), want nil, nil", lease, err)
			}
			assertShadowAdmissionIdle(t, budget)
		})
	}
}

func TestShadowAdmissionReleaseIsIdempotent(t *testing.T) {
	budget := newShadowAdmissionBudget(1024, 1)
	lease, err := budget.acquire(context.Background(), 512)
	if err != nil || lease == nil {
		t.Fatalf("acquire = (%v, %v), want lease, nil", lease, err)
	}
	lease.Release()
	lease.Release()
	assertShadowAdmissionIdle(t, budget)
}

func TestShadowAdmissionIsProcessWideAcrossMultiIndexers(t *testing.T) {
	g := graph.New()
	reg := parser.NewRegistry()
	logger := zap.NewNop()
	first := NewMultiIndexer(g, reg, search.NewNull(), nil, logger)
	second := NewMultiIndexer(g, reg, search.NewNull(), nil, logger)
	standalone := New(g, reg, config.IndexConfig{}, logger)
	if first.shadowAdmission == nil || first.shadowAdmission != second.shadowAdmission ||
		first.shadowAdmission != standalone.shadowAdmission {
		t.Fatal("indexers do not share one process-wide shadow admission gate")
	}
}

func TestShadowAdmissionWeightRejectsPriorLargeRepoShape(t *testing.T) {
	weight := shadowAdmissionWeight(35_000, 0)
	if weight <= defaultShadowProcessBudgetBytes {
		t.Fatalf("35k-file shadow charge = %d, must exceed default 1 GiB budget", weight)
	}
}

func waitForShadowWaiters(t *testing.T, budget *shadowAdmissionBudget, want int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if got := budget.snapshot().waiters; got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("shadow waiters did not reach %d; stats=%+v", want, budget.snapshot())
		}
		runtime.Gosched()
	}
}

func waitForShadowIdle(t *testing.T, budget *shadowAdmissionBudget) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		stats := budget.snapshot()
		if stats.used == 0 && stats.active == 0 && stats.waiters == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("shadow admission did not become idle: %+v", stats)
		}
		runtime.Gosched()
	}
}

func assertShadowAdmissionIdle(t *testing.T, budget *shadowAdmissionBudget) {
	t.Helper()
	stats := budget.snapshot()
	if stats.used != 0 || stats.active != 0 || stats.waiters != 0 {
		t.Fatalf("shadow admission leaked state: %+v", stats)
	}
}
