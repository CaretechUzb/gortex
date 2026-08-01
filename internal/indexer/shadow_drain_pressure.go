package indexer

import (
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

const (
	largeShadowDrainBytes     uint64 = 256 << 20
	largeShadowDrainRows             = 500_000
	largeShadowNodeReliefRows        = 65_536
	largeShadowEdgeReliefRows        = 262_144
)

// largeShadowDrainMu prevents two underestimated, edge-dense shadows from
// overlapping their live graph, SQLite drain buffers, and forced heap release.
// Ordinary shadows never acquire it.
var largeShadowDrainMu sync.Mutex

type shadowDrainPressure struct {
	enabled bool
	repo    string
	logger  *zap.Logger
	release func()

	nodeRows     int
	edgeRows     int
	nodeReleased bool
	edgeReleased bool
}

func newShadowDrainPressure(est graph.RepoMemoryEstimate, repo string, logger *zap.Logger) *shadowDrainPressure {
	return &shadowDrainPressure{
		enabled: memReleaseEnabled() && largeShadowDrainNeedsRelief(est),
		repo:    repo,
		logger:  logger,
		release: debug.FreeOSMemory,
	}
}

func largeShadowDrainNeedsRelief(est graph.RepoMemoryEstimate) bool {
	return est.Total() >= largeShadowDrainBytes || est.NodeCount+est.EdgeCount >= largeShadowDrainRows
}

// begin serializes only large shadow drains and releases parse/high-water heap
// before SQLite starts materializing its own drain working set.
func (p *shadowDrainPressure) begin() func() {
	if p == nil || !p.enabled {
		return func() {}
	}
	largeShadowDrainMu.Lock()
	p.releaseHeap("before_drain")
	var once sync.Once
	return func() {
		once.Do(largeShadowDrainMu.Unlock)
	}
}

func (p *shadowDrainPressure) afterNodeBatch(rows int) {
	if p == nil || !p.enabled || p.nodeReleased || rows <= 0 {
		return
	}
	p.nodeRows += rows
	if p.nodeRows < largeShadowNodeReliefRows {
		return
	}
	p.nodeReleased = true
	p.releaseHeap("after_node_rows")
}

func (p *shadowDrainPressure) afterEdgeBatch(rows int) {
	if p == nil || !p.enabled || p.edgeReleased || rows <= 0 {
		return
	}
	p.edgeRows += rows
	if p.edgeRows < largeShadowEdgeReliefRows {
		return
	}
	p.edgeReleased = true
	p.releaseHeap("after_edge_rows")
}

func (p *shadowDrainPressure) releaseHeap(trigger string) {
	if p == nil || p.release == nil {
		return
	}
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	started := time.Now()
	p.release()
	elapsed := time.Since(started)
	if p.logger == nil {
		return
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	p.logger.Info("indexer: released heap during large shadow drain",
		zap.String("repo", p.repo),
		zap.String("trigger", trigger),
		zap.Uint64("heap_inuse_before", before.HeapInuse),
		zap.Uint64("heap_inuse_after", after.HeapInuse),
		zap.Uint64("go_retained_before", retainedGoHeap(before)),
		zap.Uint64("go_retained_after", retainedGoHeap(after)),
		zap.Uint32("num_gc_delta", after.NumGC-before.NumGC),
		zap.Duration("elapsed", elapsed))
}

func retainedGoHeap(ms runtime.MemStats) uint64 {
	if ms.HeapReleased >= ms.HeapSys {
		return 0
	}
	return ms.HeapSys - ms.HeapReleased
}
