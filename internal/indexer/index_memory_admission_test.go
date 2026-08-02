package indexer

import (
	"context"
	"errors"
	"math"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestIndexMemoryAdmissionWeightSeparatesPressureShadowFromLargeDirect(t *testing.T) {
	// This reproduces the live cold-start shape that drove the envelope:
	// codebase-memory-mcp was a 1.30 GiB direct parse while axonhub held a
	// pressure-sized shadow. Their uncoordinated overlap produced a 6.27 GiB
	// heap high-water and 14 one-second passive-checkpoint timeouts, versus zero
	// checkpoint timeouts in the sealed run.
	const (
		directFiles        = 1776
		directBytes        = int64(1_297_649_391)
		observedAxonWeight = int64(346_946_586)
	)

	directWeight := indexMemoryAdmissionWeight(directFiles, directBytes)
	pressureWeight := observedAxonWeight + indexMemoryAdmissionRepoOverhead
	if directWeight != 2_895_191_518 {
		t.Fatalf("direct weight = %d, want 2895191518", directWeight)
	}
	if pressureWeight != 414_055_450 {
		t.Fatalf("pressure weight = %d, want 414055450", pressureWeight)
	}
	if directWeight+pressureWeight <= defaultIndexMemoryAdmissionBytes {
		t.Fatalf("large direct + pressure shadow unexpectedly fit: %d + %d <= %d",
			directWeight, pressureWeight, defaultIndexMemoryAdmissionBytes)
	}

	smallWeight := indexMemoryAdmissionWeight(1, 1)
	if directWeight+smallWeight > defaultIndexMemoryAdmissionBytes {
		t.Fatalf("large direct should retain headroom for small overlap: %d + %d > %d",
			directWeight, smallWeight, defaultIndexMemoryAdmissionBytes)
	}
}

func TestIndexMemoryAdmissionQueuedPressureAllowsBoundedSmallOverlap(t *testing.T) {
	budget := newIndexMemoryAdmissionBudget(defaultIndexMemoryAdmissionBytes)
	directWeight := indexMemoryAdmissionWeight(1776, 1_297_649_391)
	pressureWeight := int64(346_946_586) + indexMemoryAdmissionRepoOverhead
	smallWeight := indexMemoryAdmissionWeight(1, 1)

	direct, err := budget.acquire(t.Context(), directWeight)
	if err != nil {
		t.Fatal(err)
	}
	pressureGranted := make(chan *indexMemoryAdmissionLease, 1)
	go func() {
		lease, _ := budget.acquire(t.Context(), pressureWeight)
		pressureGranted <- lease
	}()
	waitForIndexMemoryWaiters(t, budget, 1)

	// The later small repository fits in the direct parse's remaining 326 MiB.
	// It must bypass the queued pressure shadow instead of being stranded behind
	// the FIFO head as semaphore.Weighted did.
	smallCtx, cancelSmall := context.WithTimeout(t.Context(), time.Second)
	defer cancelSmall()
	small, err := budget.acquire(smallCtx, smallWeight)
	if err != nil {
		t.Fatalf("fitting small repository failed to bypass pressure waiter: %v", err)
	}
	select {
	case lease := <-pressureGranted:
		lease.Release()
		t.Fatal("pressure shadow admitted while large direct parse was live")
	default:
	}

	small.Release()
	direct.Release()
	select {
	case pressure := <-pressureGranted:
		if pressure == nil {
			t.Fatal("pressure admission unexpectedly failed")
		}
		pressure.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("pressure shadow did not enter after large direct release")
	}

	stats := budget.snapshot()
	if stats.used != 0 || stats.waiters != 0 || stats.bypasses != 0 {
		t.Fatalf("budget not drained: %+v", stats)
	}
	if stats.admissions != 3 || stats.queued != 2 {
		t.Fatalf("unexpected admission telemetry: %+v", stats)
	}
	if stats.peak > stats.capacity {
		t.Fatalf("peak exceeded capacity: %+v", stats)
	}
}

func TestIndexMemoryAdmissionBypassBoundGuaranteesHeadProgress(t *testing.T) {
	budget := newIndexMemoryAdmissionBudget(4)
	held, err := budget.acquire(t.Context(), 2)
	if err != nil {
		t.Fatal(err)
	}

	headDone := make(chan *indexMemoryAdmissionLease, 1)
	go func() {
		lease, _ := budget.acquire(t.Context(), 4)
		headDone <- lease
	}()
	waitForIndexMemoryWaiters(t, budget, 1)

	for range maxIndexMemoryAdmissionBypasses {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		small, acquireErr := budget.acquire(ctx, 1)
		cancel()
		if acquireErr != nil {
			t.Fatalf("bounded fitting bypass failed: %v", acquireErr)
		}
		small.Release()
	}
	if got := budget.snapshot().bypasses; got != maxIndexMemoryAdmissionBypasses {
		t.Fatalf("bypasses = %d, want %d", got, maxIndexMemoryAdmissionBypasses)
	}

	lateDone := make(chan *indexMemoryAdmissionLease, 1)
	go func() {
		lease, _ := budget.acquire(t.Context(), 1)
		lateDone <- lease
	}()
	waitForIndexMemoryWaiters(t, budget, 2)
	select {
	case lease := <-lateDone:
		lease.Release()
		t.Fatal("small repository bypassed after starvation bound was spent")
	default:
	}

	held.Release()
	head := <-headDone
	if head == nil {
		t.Fatal("FIFO head did not acquire after capacity release")
	}
	select {
	case lease := <-lateDone:
		lease.Release()
		t.Fatal("later small repository passed the active capacity-sized head")
	default:
	}
	head.Release()
	select {
	case late := <-lateDone:
		if late == nil {
			t.Fatal("later small admission unexpectedly failed")
		}
		late.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("later small repository did not progress after FIFO head")
	}
}

func TestIndexMemoryAdmissionCancellationAndIdempotentRelease(t *testing.T) {
	budget := newIndexMemoryAdmissionBudget(100)
	full, err := budget.acquire(t.Context(), 100)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, acquireErr := budget.acquire(ctx, 60)
		result <- acquireErr
	}()
	waitForIndexMemoryWaiters(t, budget, 1)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("acquire error = %v, want context cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled admission did not return")
	}

	full.Release()
	full.Release()
	stats := budget.snapshot()
	if stats.used != 0 || stats.waiters != 0 || stats.admissions != 1 || stats.queued != 1 {
		t.Fatalf("unexpected final telemetry: %+v", stats)
	}

	cancelled, cancelNow := context.WithCancel(t.Context())
	cancelNow()
	if _, err := budget.acquire(cancelled, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled acquire error = %v, want context cancellation", err)
	}
}

func TestIndexMemoryAdmissionCancellationWinsGrantRace(t *testing.T) {
	for range 50 {
		budget := newIndexMemoryAdmissionBudget(1)
		held, err := budget.acquire(t.Context(), 1)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() {
			lease, acquireErr := budget.acquire(ctx, 1)
			if lease != nil {
				lease.Release()
			}
			result <- acquireErr
		}()
		waitForIndexMemoryWaiters(t, budget, 1)
		cancel()
		held.Release()
		if err := <-result; !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel/grant race error = %v, want cancellation", err)
		}
		probe, err := budget.acquire(t.Context(), 1)
		if err != nil {
			t.Fatalf("cancel/grant race leaked capacity: %v", err)
		}
		probe.Release()
	}
}

func TestIndexMemoryAdmissionConcurrentInvariant(t *testing.T) {
	const capacity = int64(8)
	budget := newIndexMemoryAdmissionBudget(capacity)
	var active atomic.Int64
	var violated atomic.Bool
	var wg sync.WaitGroup
	for worker := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := range 100 {
				weight := int64(1 + (worker+iteration)%3)
				lease, err := budget.acquire(t.Context(), weight)
				if err != nil {
					violated.Store(true)
					return
				}
				if active.Add(weight) > capacity {
					violated.Store(true)
				}
				runtime.Gosched()
				active.Add(-weight)
				lease.Release()
			}
		}()
	}
	wg.Wait()
	if violated.Load() || active.Load() != 0 {
		t.Fatalf("concurrent admission invariant violated: active=%d stats=%+v", active.Load(), budget.snapshot())
	}
	stats := budget.snapshot()
	if stats.used != 0 || stats.waiters != 0 || stats.peak > capacity {
		t.Fatalf("invalid final budget state: %+v", stats)
	}
}

func TestIndexMemoryAdmissionConfigurationAndSaturation(t *testing.T) {
	t.Setenv("GORTEX_INDEX_MEMORY_BUDGET_BYTES", "12345")
	if got := indexMemoryAdmissionBytes(); got != 12345 {
		t.Fatalf("configured budget = %d, want 12345", got)
	}
	t.Setenv("GORTEX_INDEX_MEMORY_BUDGET_BYTES", "0")
	if got := indexMemoryAdmissionBytes(); got != 0 {
		t.Fatalf("disabled budget = %d, want 0", got)
	}
	for _, raw := range []string{"invalid", "-1"} {
		t.Setenv("GORTEX_INDEX_MEMORY_BUDGET_BYTES", raw)
		if got := indexMemoryAdmissionBytes(); got != defaultIndexMemoryAdmissionBytes {
			t.Fatalf("budget for %q = %d, want default %d", raw, got, defaultIndexMemoryAdmissionBytes)
		}
	}

	if got := indexMemoryAdmissionWeight(math.MaxInt, math.MaxInt64); got != math.MaxInt64 {
		t.Fatalf("saturated weight = %d, want %d", got, int64(math.MaxInt64))
	}
	if got := saturatingAddByteCount(math.MaxInt64-1, 2); got != math.MaxInt64 {
		t.Fatalf("saturated byte sum = %d, want %d", got, int64(math.MaxInt64))
	}
	if got := saturatingAddByteCount(-1, 7); got != 7 {
		t.Fatalf("negative byte sum recovery = %d, want 7", got)
	}
	if got := newIndexMemoryAdmissionBudget(-1).snapshot().capacity; got != 0 {
		t.Fatalf("negative capacity = %d, want 0", got)
	}
}

func waitForIndexMemoryWaiters(t *testing.T, budget *indexMemoryAdmissionBudget, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if budget.snapshot().waiters == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("waiters = %d, want %d", budget.snapshot().waiters, want)
}

func BenchmarkIndexMemoryAdmissionUncontended(b *testing.B) {
	budget := newIndexMemoryAdmissionBudget(defaultIndexMemoryAdmissionBytes)
	weight := indexMemoryAdmissionWeight(1, 1)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		lease, err := budget.acquire(ctx, weight)
		if err != nil {
			b.Fatal(err)
		}
		lease.Release()
	}
}
