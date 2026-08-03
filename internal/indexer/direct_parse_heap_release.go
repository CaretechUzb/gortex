package indexer

import (
	"runtime"
	"runtime/debug"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// A direct parse must be large independently of operator shadow overrides.
	// This keeps an intentionally lowered shadow threshold from forcing GC for
	// ordinary repositories that merely lost the process-wide shadow race.
	// Reclaim before a second pressure-sized direct parse stacks on the first
	// parse's idle spans. The previous 2 GiB threshold missed a measured 4.9 GiB
	// physical-footprint peak despite only 186 MiB of live heap. Eligibility,
	// serialization, and the cooldown below keep this 512 MiB boundary narrow.
	directParseHeapReleaseMinRetained uint64 = 512 << 20
	directParseHeapReleaseCooldown           = 30 * time.Second
)

type directParseHeapReleaseRequest struct {
	directStore bool
	repo        string
	files       int
	inputBytes  int64
	logger      *zap.Logger
}

// directParseHeapReleaser serializes the expensive GC+scavenge across every
// repository in the process. Only large, direct-to-durable-store parses reach
// this controller; the retained-heap check and cooldown run again under the
// lock so concurrent completions cannot issue redundant releases.
type directParseHeapReleaser struct {
	mu          sync.Mutex
	lastRelease time.Time

	minRetained uint64
	cooldown    time.Duration
	now         func() time.Time
	readStats   func(*runtime.MemStats)
	release     func()
}

var processDirectParseHeapReleaser = newDirectParseHeapReleaser()

func newDirectParseHeapReleaser() *directParseHeapReleaser {
	return &directParseHeapReleaser{
		minRetained: directParseHeapReleaseMinRetained,
		cooldown:    directParseHeapReleaseCooldown,
		now:         time.Now,
		readStats:   runtime.ReadMemStats,
		release:     debug.FreeOSMemory,
	}
}

func maybeReleaseHeapAfterLargeDirectParse(
	directStore bool,
	repo string,
	files int,
	inputBytes int64,
	logger *zap.Logger,
) bool {
	if !memReleaseEnabled() {
		return false
	}
	return processDirectParseHeapReleaser.maybeRelease(directParseHeapReleaseRequest{
		directStore: directStore,
		repo:        repo,
		files:       files,
		inputBytes:  inputBytes,
		logger:      logger,
	})
}

func largeDirectParseNeedsHeapRelease(directStore bool, files int, inputBytes int64) bool {
	if !directStore {
		return false
	}
	return files >= defaultShadowMaxFileCount || inputBytes >= defaultShadowMaxBytes
}

func (r *directParseHeapReleaser) maybeRelease(req directParseHeapReleaseRequest) bool {
	if r == nil || r.now == nil || r.readStats == nil || r.release == nil ||
		!largeDirectParseNeedsHeapRelease(req.directStore, req.files, req.inputBytes) {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	if !r.lastRelease.IsZero() && now.Before(r.lastRelease.Add(r.cooldown)) {
		return false
	}

	var before runtime.MemStats
	r.readStats(&before)
	retainedBefore := retainedGoHeap(before)
	if retainedBefore < r.minRetained {
		return false
	}

	started := now
	r.release()
	finished := r.now()
	r.lastRelease = finished

	var after runtime.MemStats
	r.readStats(&after)
	if req.logger != nil {
		req.logger.Info("indexer: released heap after large direct parse",
			zap.String("repo", req.repo),
			zap.Int("files", req.files),
			zap.Int64("input_bytes", req.inputBytes),
			zap.Uint64("heap_inuse_before", before.HeapInuse),
			zap.Uint64("heap_inuse_after", after.HeapInuse),
			zap.Uint64("go_retained_before", retainedBefore),
			zap.Uint64("go_retained_after", retainedGoHeap(after)),
			zap.Uint32("num_gc_delta", after.NumGC-before.NumGC),
			zap.Duration("elapsed", finished.Sub(started)))
	}
	return true
}
