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

type directAdmissionTestStore struct {
	graph.Store
}

func (*directAdmissionTestStore) BulkSetFileMtimes(string, map[string]int64) error {
	return nil
}

func TestLargeDirectParseAdmissionWeightSerializesEveryFullRepository(t *testing.T) {
	direct := &directAdmissionTestStore{Store: graph.New()}
	inMemory := graph.New()

	// Store shape, streaming mode, and operator shadow thresholds must not let
	// a non-empty full-tree parse overlap another repository's parse phase.
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "1")
	t.Setenv("GORTEX_SHADOW_MAX_BYTES", "1")

	tests := []struct {
		name       string
		store      graph.Store
		streaming  bool
		files      int
		inputBytes int64
		want       int64
	}{
		{
			name:       "ordinary direct repository takes capacity",
			store:      direct,
			files:      defaultShadowMaxFileCount - 1,
			inputBytes: defaultShadowMaxBytes - 1,
			want:       largeDirectAdmissionCapacity,
		},
		{
			name:       "large direct repository takes capacity",
			store:      direct,
			files:      defaultShadowMaxFileCount,
			inputBytes: defaultShadowMaxBytes,
			want:       largeDirectAdmissionCapacity,
		},
		{
			name:  "in-memory shadow takes capacity",
			store: inMemory,
			files: 1,
			want:  largeDirectAdmissionCapacity,
		},
		{
			name:      "streaming shadow takes capacity",
			store:     direct,
			streaming: true,
			files:     1,
			want:      largeDirectAdmissionCapacity,
		},
		{
			name:  "nil store still takes capacity",
			files: 1,
			want:  largeDirectAdmissionCapacity,
		},
		{
			name:  "empty repository bypasses",
			store: direct,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := largeDirectParseAdmissionWeight(tt.store, tt.streaming, tt.files, tt.inputBytes)
			if got != tt.want {
				t.Fatalf("largeDirectParseAdmissionWeight() = %d, want %d", got, tt.want)
			}
		})
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
