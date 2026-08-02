package indexer

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

type admissionBulkStore struct {
	graph.Store
}

func (*admissionBulkStore) BeginBulkLoad()   {}
func (*admissionBulkStore) FlushBulk() error { return nil }

func TestLargeDirectParseAdmissionWeightPreservesBoundedParallelism(t *testing.T) {
	memoryStore := graph.New()
	durableStore := &admissionBulkStore{Store: graph.New()}

	tests := []struct {
		name                string
		store               graph.Store
		streaming           bool
		files               int
		inputBytes          int64
		nativePressureFiles int
		nativePressureBytes int64
		want                int64
	}{
		{name: "empty durable parse", store: durableStore},
		{name: "small durable parse", store: durableStore, files: 1, inputBytes: 1},
		{name: "large in-memory shadow", store: memoryStore, files: defaultShadowMaxFileCount, inputBytes: defaultShadowMaxBytes},
		{name: "large streaming parse", store: durableStore, streaming: true, files: defaultShadowMaxFileCount, inputBytes: defaultShadowMaxBytes},
		{name: "large lightweight file count", store: durableStore, files: defaultShadowMaxFileCount, want: largeDirectLightweightParseWeight},
		{name: "large lightweight byte count", store: durableStore, files: 1, inputBytes: defaultShadowMaxBytes, want: largeDirectLightweightParseWeight},
		{name: "large native byte pressure", store: durableStore, files: 1, inputBytes: defaultShadowMaxBytes, nativePressureBytes: nativeParsePressureThresholdBytes, want: largeDirectNativePressureWeight},
		{name: "large native file pressure", store: durableStore, files: defaultShadowMaxFileCount, nativePressureFiles: defaultShadowMaxFileCount, want: largeDirectNativePressureWeight},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := largeDirectParseAdmissionWeight(
				tt.store, tt.streaming, tt.files, tt.inputBytes,
				tt.nativePressureFiles, tt.nativePressureBytes,
			)
			if got != tt.want {
				t.Fatalf("largeDirectParseAdmissionWeight() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestLargeDirectAdmissionAllowsOneLightAlongsideNativePressure(t *testing.T) {
	budget := newLargeDirectAdmissionBudget(largeDirectAdmissionCapacity)
	pressure, err := budget.acquire(t.Context(), largeDirectNativePressureWeight)
	if err != nil {
		t.Fatal(err)
	}
	defer pressure.Release()

	light, err := budget.acquire(t.Context(), largeDirectLightweightParseWeight)
	if err != nil {
		t.Fatalf("light parse did not overlap native pressure: %v", err)
	}

	blocked := make(chan *largeDirectAdmissionLease, 1)
	go func() {
		lease, _ := budget.acquire(t.Context(), largeDirectLightweightParseWeight)
		blocked <- lease
	}()
	waitForLargeDirectAdmission(t, budget, func(stats largeDirectAdmissionStats) bool {
		return stats.used == largeDirectAdmissionCapacity && stats.waiters == 1
	})
	select {
	case lease := <-blocked:
		lease.Release()
		t.Fatal("second light parse exceeded bounded overlap")
	default:
	}

	light.Release()
	select {
	case lease := <-blocked:
		if lease == nil {
			t.Fatal("queued light parse was not granted")
		}
		lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("queued light parse did not enter after permit release")
	}
}

func TestLargeDirectAdmissionSerializesAndReleasesIdempotently(t *testing.T) {
	budget := newLargeDirectAdmissionBudget(largeDirectAdmissionCapacity)
	first, err := budget.acquire(context.Background(), largeDirectAdmissionCapacity)
	if err != nil {
		t.Fatalf("acquire first lease: %v", err)
	}

	type acquireResult struct {
		lease *largeDirectAdmissionLease
		err   error
	}
	secondResult := make(chan acquireResult, 1)
	go func() {
		lease, err := budget.acquire(context.Background(), largeDirectAdmissionCapacity)
		secondResult <- acquireResult{lease: lease, err: err}
	}()
	waitForLargeDirectAdmission(t, budget, func(stats largeDirectAdmissionStats) bool {
		return stats.used == largeDirectAdmissionCapacity && stats.waiters == 1
	})
	select {
	case result := <-secondResult:
		if result.lease != nil {
			result.lease.Release()
		}
		t.Fatalf("second acquire completed before release: %v", result.err)
	default:
	}

	first.Release()
	first.Release() // idempotence: must not over-release the weighted semaphore.
	select {
	case result := <-secondResult:
		if result.err != nil {
			t.Fatalf("acquire second lease: %v", result.err)
		}
		result.lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("second acquire did not enter after first release")
	}

	stats := budget.snapshot()
	if stats.used != 0 || stats.waiters != 0 || stats.peak != largeDirectAdmissionCapacity {
		t.Fatalf("unexpected final stats: %+v", stats)
	}
}

func TestLargeDirectAdmissionCancellationReturnsPermit(t *testing.T) {
	budget := newLargeDirectAdmissionBudget(largeDirectAdmissionCapacity)
	first, err := budget.acquire(context.Background(), largeDirectAdmissionCapacity)
	if err != nil {
		t.Fatalf("acquire first lease: %v", err)
	}
	defer first.Release()

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := budget.acquire(ctx, largeDirectAdmissionCapacity)
		result <- err
	}()
	waitForLargeDirectAdmission(t, budget, func(stats largeDirectAdmissionStats) bool {
		return stats.waiters == 1
	})
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled acquire error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled acquire did not return")
	}
	waitForLargeDirectAdmission(t, budget, func(stats largeDirectAdmissionStats) bool {
		return stats.used == largeDirectAdmissionCapacity && stats.waiters == 0
	})

	// Zero-weight work bypasses an occupied large lane.
	bypass, err := budget.acquire(context.Background(), 0)
	if err != nil || bypass != nil {
		t.Fatalf("zero-weight acquire = (%v, %v), want (nil, nil)", bypass, err)
	}
}

func TestLargeDirectAdmissionDoesNotBlockPressureShadowDrain(t *testing.T) {
	budget := newLargeDirectAdmissionBudget(largeDirectAdmissionCapacity)
	direct, err := budget.acquire(context.Background(), largeDirectAdmissionCapacity)
	if err != nil {
		t.Fatalf("acquire direct lease: %v", err)
	}
	defer direct.Release()

	ctx, cancel := context.WithCancel(context.Background())
	queued := make(chan error, 1)
	go func() {
		lease, err := budget.acquire(ctx, largeDirectAdmissionCapacity)
		if lease != nil {
			lease.Release()
		}
		queued <- err
	}()
	waitForLargeDirectAdmission(t, budget, func(stats largeDirectAdmissionStats) bool {
		return stats.used == largeDirectAdmissionCapacity && stats.waiters == 1
	})

	drainStarted := make(chan func(), 1)
	go func() {
		drain := &shadowDrainPressure{enabled: true, release: func() {}}
		drainStarted <- drain.begin()
	}()
	select {
	case finishDrain := <-drainStarted:
		finishDrain()
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("pressure shadow drain was blocked by large direct admission")
	}

	cancel()
	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled direct waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled direct waiter did not return")
	}
	waitForLargeDirectAdmission(t, budget, func(stats largeDirectAdmissionStats) bool {
		return stats.used == largeDirectAdmissionCapacity && stats.waiters == 0
	})
}

func TestLargeDirectAdmissionRaceStress(t *testing.T) {
	budget := newLargeDirectAdmissionBudget(largeDirectAdmissionCapacity)
	var active atomic.Int64
	var peak atomic.Int64
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				lease, err := budget.acquire(context.Background(), largeDirectAdmissionCapacity)
				if err != nil {
					t.Errorf("acquire: %v", err)
					return
				}
				now := active.Add(1)
				for {
					old := peak.Load()
					if now <= old || peak.CompareAndSwap(old, now) {
						break
					}
				}
				runtime.Gosched()
				active.Add(-1)
				lease.Release()
			}
		}()
	}
	wg.Wait()
	if got := peak.Load(); got != 1 {
		t.Fatalf("maximum concurrent large parses = %d, want 1", got)
	}
	stats := budget.snapshot()
	if stats.used != 0 || stats.waiters != 0 {
		t.Fatalf("gate leaked after stress: %+v", stats)
	}
}

func waitForLargeDirectAdmission(
	t *testing.T,
	budget *largeDirectAdmissionBudget,
	ready func(largeDirectAdmissionStats) bool,
) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		stats := budget.snapshot()
		if ready(stats) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("admission state did not converge: %+v", stats)
		}
		runtime.Gosched()
	}
}
