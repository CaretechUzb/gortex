package main

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/runtimeactivity"
)

type healthGraphScanSpy struct {
	graph.Store
	memoryEstimateCalls atomic.Int32
	nodeCountCalls      atomic.Int32
	edgeCountCalls      atomic.Int32
}

func (s *healthGraphScanSpy) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	s.memoryEstimateCalls.Add(1)
	return nil
}

func (s *healthGraphScanSpy) NodeCount() int {
	s.nodeCountCalls.Add(1)
	return 1
}

func (s *healthGraphScanSpy) EdgeCount() int {
	s.edgeCountCalls.Add(1)
	return 1
}

func TestDaemonHealthSnapshotIsQueryFreeAndActivityNeutral(t *testing.T) {
	spy := &healthGraphScanSpy{}
	controller := &realController{graph: spy}
	controller.ready.Store(true)
	controller.enriched.Store(true)
	controller.warmupSeconds.Store(12)
	state := &daemonState{graph: spy}

	before := runtimeactivity.Current()
	snapshot := buildDaemonHealthSnapshot(time.Now().Add(-time.Minute), controller, state, nil)
	after := runtimeactivity.Current()

	if got := spy.memoryEstimateCalls.Load(); got != 0 {
		t.Fatalf("health memory-estimate calls = %d, want 0", got)
	}
	if got := spy.nodeCountCalls.Load(); got != 0 {
		t.Fatalf("health node-count calls = %d, want 0", got)
	}
	if got := spy.edgeCountCalls.Load(); got != 0 {
		t.Fatalf("health edge-count calls = %d, want 0", got)
	}
	if after.Epoch != before.Epoch {
		t.Fatalf("health snapshot advanced activity epoch from %d to %d", before.Epoch, after.Epoch)
	}
	if got := snapshot["warmup_seconds"]; got != int64(12) {
		t.Fatalf("warmup_seconds = %v, want 12", got)
	}
	if got, ok := snapshot["alloc_bytes"].(uint64); !ok || got == 0 {
		t.Fatalf("alloc_bytes = %#v, want a non-zero runtime metric", snapshot["alloc_bytes"])
	}
	if got, ok := snapshot["num_goroutine"].(int); !ok || got < 1 {
		t.Fatalf("num_goroutine = %#v, want at least one", snapshot["num_goroutine"])
	}
}
