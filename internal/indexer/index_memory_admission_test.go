package indexer

import (
	"context"
	"errors"
	"math"
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

func TestIndexMemoryAdmissionBudgetPreservesSmallOverlapAndQueuesPressureWork(t *testing.T) {
	budget := newIndexMemoryAdmissionBudget(defaultIndexMemoryAdmissionBytes)
	directWeight := indexMemoryAdmissionWeight(1776, 1_297_649_391)
	pressureWeight := int64(346_946_586) + indexMemoryAdmissionRepoOverhead
	smallWeight := indexMemoryAdmissionWeight(1, 1)

	direct, err := budget.acquire(context.Background(), directWeight)
	if err != nil {
		t.Fatal(err)
	}
	small, err := budget.acquire(context.Background(), smallWeight)
	if err != nil {
		t.Fatal(err)
	}

	granted := make(chan *indexMemoryAdmissionLease, 1)
	errs := make(chan error, 1)
	go func() {
		lease, acquireErr := budget.acquire(context.Background(), pressureWeight)
		if acquireErr != nil {
			errs <- acquireErr
			return
		}
		granted <- lease
	}()
	waitForIndexMemoryWaiters(t, budget, 1)

	select {
	case lease := <-granted:
		lease.Release()
		t.Fatal("pressure shadow admitted while large direct parse was live")
	case err := <-errs:
		t.Fatalf("pressure admission failed: %v", err)
	default:
	}

	// Releasing only the small overlap still leaves insufficient capacity for
	// the pressure shadow at the FIFO head.
	small.Release()
	select {
	case lease := <-granted:
		lease.Release()
		t.Fatal("pressure shadow admitted before large direct release")
	case err := <-errs:
		t.Fatalf("pressure admission failed: %v", err)
	default:
	}

	direct.Release()
	select {
	case lease := <-granted:
		lease.Release()
	case err := <-errs:
		t.Fatalf("pressure admission failed: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("pressure shadow did not enter after large direct release")
	}

	stats := budget.snapshot()
	if stats.used != 0 || stats.waiters != 0 {
		t.Fatalf("budget not drained: %+v", stats)
	}
	if stats.admissions != 3 || stats.queued != 1 {
		t.Fatalf("unexpected admission telemetry: %+v", stats)
	}
	if stats.peak > stats.capacity {
		t.Fatalf("peak exceeded capacity: %+v", stats)
	}
}

func TestIndexMemoryAdmissionCancellationAndIdempotentRelease(t *testing.T) {
	budget := newIndexMemoryAdmissionBudget(100)
	full, err := budget.acquire(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
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

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	if _, err := budget.acquire(cancelled, 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled acquire error = %v, want context cancellation", err)
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
