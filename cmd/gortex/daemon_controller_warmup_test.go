package main

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type statusAggregateSpy struct {
	graph.Store
	memoryEstimateCalls atomic.Int32
	nodeCountCalls      atomic.Int32
	edgeCountCalls      atomic.Int32
}

func (s *statusAggregateSpy) AllRepoMemoryEstimates() map[string]graph.RepoMemoryEstimate {
	s.memoryEstimateCalls.Add(1)
	return map[string]graph.RepoMemoryEstimate{
		"repo": {NodeCount: 11, EdgeCount: 17},
	}
}

func (s *statusAggregateSpy) NodeCount() int {
	s.nodeCountCalls.Add(1)
	return 11
}

func (s *statusAggregateSpy) EdgeCount() int {
	s.edgeCountCalls.Add(1)
	return 17
}

func TestControllerStatusDefersGraphAggregatesUntilEnriched(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	spy := &statusAggregateSpy{}
	controller := &realController{graph: spy}

	status, err := controller.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.EnrichmentComplete {
		t.Fatal("warming controller reported enrichment complete")
	}
	if got := spy.memoryEstimateCalls.Load(); got != 0 {
		t.Fatalf("warming status memory-estimate calls = %d, want 0", got)
	}
	if got := spy.nodeCountCalls.Load(); got != 0 {
		t.Fatalf("warming status node-count calls = %d, want 0", got)
	}
	if got := spy.edgeCountCalls.Load(); got != 0 {
		t.Fatalf("warming status edge-count calls = %d, want 0", got)
	}

	controller.enriched.Store(true)
	status, err = controller.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.EnrichmentComplete {
		t.Fatal("enriched controller reported warming status")
	}
	if got := spy.memoryEstimateCalls.Load(); got != 1 {
		t.Fatalf("enriched status memory-estimate calls = %d, want 1", got)
	}
	if got := spy.nodeCountCalls.Load(); got != 1 {
		t.Fatalf("enriched status node-count calls = %d, want 1", got)
	}
	if got := spy.edgeCountCalls.Load(); got != 1 {
		t.Fatalf("enriched status edge-count calls = %d, want 1", got)
	}
}
