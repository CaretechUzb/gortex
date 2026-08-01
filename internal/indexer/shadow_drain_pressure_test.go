package indexer

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestLargeShadowDrainNeedsRelief(t *testing.T) {
	tests := []struct {
		name string
		est  graph.RepoMemoryEstimate
		want bool
	}{
		{name: "ordinary", est: graph.RepoMemoryEstimate{NodeBytes: 64 << 20, EdgeBytes: 64 << 20, NodeCount: 20_000, EdgeCount: 100_000}},
		{name: "bytes", est: graph.RepoMemoryEstimate{NodeBytes: largeShadowDrainBytes}, want: true},
		{name: "rows", est: graph.RepoMemoryEstimate{NodeCount: 100_000, EdgeCount: largeShadowDrainRows - 100_000}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := largeShadowDrainNeedsRelief(tt.est); got != tt.want {
				t.Fatalf("largeShadowDrainNeedsRelief(%+v) = %v, want %v", tt.est, got, tt.want)
			}
		})
	}
}

func TestShadowDrainPressureReclaimsOncePerPhase(t *testing.T) {
	var releases atomic.Int64
	p := &shadowDrainPressure{
		enabled: true,
		release: func() { releases.Add(1) },
	}
	unlock := p.begin()
	defer unlock()
	if got := releases.Load(); got != 1 {
		t.Fatalf("pre-drain releases = %d, want 1", got)
	}

	p.afterNodeBatch(largeShadowNodeReliefRows - 1)
	if got := releases.Load(); got != 1 {
		t.Fatalf("premature node release count = %d, want 1", got)
	}
	p.afterNodeBatch(1)
	p.afterNodeBatch(largeShadowNodeReliefRows)
	if got := releases.Load(); got != 2 {
		t.Fatalf("node release count = %d, want 2", got)
	}

	p.afterEdgeBatch(largeShadowEdgeReliefRows - 1)
	if got := releases.Load(); got != 2 {
		t.Fatalf("premature edge release count = %d, want 2", got)
	}
	p.afterEdgeBatch(1)
	p.afterEdgeBatch(largeShadowEdgeReliefRows)
	if got := releases.Load(); got != 3 {
		t.Fatalf("edge release count = %d, want 3", got)
	}
}

func TestLargeShadowDrainsSerialize(t *testing.T) {
	first := &shadowDrainPressure{enabled: true, release: func() {}}
	unlockFirst := first.begin()

	acquired := make(chan func(), 1)
	go func() {
		second := &shadowDrainPressure{enabled: true, release: func() {}}
		acquired <- second.begin()
	}()
	select {
	case unlockSecond := <-acquired:
		unlockSecond()
		unlockFirst()
		t.Fatal("second large shadow drain acquired guard concurrently")
	case <-time.After(50 * time.Millisecond):
	}

	unlockFirst()
	select {
	case unlockSecond := <-acquired:
		unlockSecond()
	case <-time.After(2 * time.Second):
		t.Fatal("second large shadow drain did not acquire guard after release")
	}
}

func TestOrdinaryShadowDrainBypassesPressure(t *testing.T) {
	var releases atomic.Int64
	p := &shadowDrainPressure{release: func() { releases.Add(1) }}
	unlock := p.begin()
	p.afterNodeBatch(largeShadowNodeReliefRows)
	p.afterEdgeBatch(largeShadowEdgeReliefRows)
	unlock()
	if got := releases.Load(); got != 0 {
		t.Fatalf("ordinary shadow triggered %d releases, want 0", got)
	}
}
