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

func TestIndexMemoryAdmissionWeightChargesCompleteDirectCorpus(t *testing.T) {
	// This reproduces the measured cold-start pressure shape. Direct graph
	// batches are bounded, but repository-scale parser and SQLite work still
	// overlap unless the complete corpus participates in admission. Language
	// composition must therefore not turn a large repository into a cheap lane.
	const (
		nativePressureFiles = 1505
		nativePressureBytes = int64(1_291_747_571)
		nonNativeFiles      = 10_000
		nonNativeBytes      = int64(2_170_000_000)
		observedAxonWeight  = int64(346_946_586)
	)

	directWeight := directIndexMemoryAdmissionWeight(nativePressureFiles, nativePressureBytes)
	nonNativeWeight := directIndexMemoryAdmissionWeight(nonNativeFiles, nonNativeBytes)
	pressureWeight := observedAxonWeight + indexMemoryAdmissionRepoOverhead
	if directWeight != 2_847_867_366 {
		t.Fatalf("pressure direct weight = %d, want 2847867366", directWeight)
	}
	if want := indexMemoryAdmissionWeight(nonNativeFiles, nonNativeBytes); nonNativeWeight != want {
		t.Fatalf("non-native direct weight = %d, want complete-corpus weight %d", nonNativeWeight, want)
	}
	if pressureWeight != 414_055_450 {
		t.Fatalf("pressure weight = %d, want 414055450", pressureWeight)
	}
	if directWeight+pressureWeight <= defaultIndexMemoryAdmissionBytes {
		t.Fatalf("pressure direct + pressure shadow unexpectedly fit: %d + %d <= %d",
			directWeight, pressureWeight, defaultIndexMemoryAdmissionBytes)
	}
}

func TestIndexMemoryAdmissionQueuedPressureAllowsBoundedSmallOverlap(t *testing.T) {
	budget := newIndexMemoryAdmissionBudget(defaultIndexMemoryAdmissionBytes)
	directWeight := directIndexMemoryAdmissionWeight(1505, 1_291_747_571)
	pressureWeight := int64(346_946_586) + indexMemoryAdmissionRepoOverhead
	smallWeight := directIndexMemoryAdmissionWeight(0, 0)

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

	// A genuinely tiny direct repository fits in the native-pressure parse's
	// remaining 356 MiB. It may bypass the queued pressure shadow within the
	// starvation bound instead of leaving capacity idle.
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
