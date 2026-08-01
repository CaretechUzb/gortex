package indexer

import (
	"sync/atomic"
	"time"
)

// nativeParsePressureThresholdBytes amortises a native allocator sweep across
// enough completed source to matter. Tree-sitter's C trees can temporarily use
// many times the raw source size; sweeping every file would serialize the hot
// parse path, while 64 MiB keeps the retained-zone high-water bounded on large
// generated C/C++ corpora.
const nativeParsePressureThresholdBytes int64 = 64 << 20

type nativeParsePressureStats struct {
	calls         int64
	releasedBytes uint64
	elapsed       time.Duration
}

type nativeParsePressureRelief struct {
	pending  atomic.Int64
	sweeping atomic.Bool
	calls    atomic.Int64
	released atomic.Uint64
	elapsed  atomic.Int64
	release  func() uintptr
}

func newNativeParsePressureRelief() *nativeParsePressureRelief {
	if !nativeAllocatorPressureReliefSupported {
		return nil
	}
	return &nativeParsePressureRelief{release: nativeAllocatorPressureRelief}
}

func nativeParsePressureLanguage(language string) bool {
	switch language {
	case "c", "cpp", "cuda", "objc":
		return true
	default:
		return false
	}
}

// afterParse records one in-process tree-sitter parse after its extractor has
// returned and closed the native tree. Only C-family grammars participate: the
// measured cold-index high-water came from their large generated syntax trees,
// and broad allocator sweeps on ordinary small-language parses would add
// latency without reclaiming meaningful memory.
func (r *nativeParsePressureRelief) afterParse(language string, sourceBytes int64) {
	if r == nil || sourceBytes <= 0 || !nativeParsePressureLanguage(language) {
		return
	}
	if r.pending.Add(sourceBytes) < nativeParsePressureThresholdBytes {
		return
	}
	r.relieve(false)
}

// flush releases the final under-threshold tail once every parser worker has
// exited. It is deliberately called at the parse boundary, not on an idle
// timer, so steady-state query latency is unaffected.
func (r *nativeParsePressureRelief) flush() {
	if r == nil || r.pending.Load() <= 0 {
		return
	}
	r.relieve(true)
}

func (r *nativeParsePressureRelief) relieve(force bool) {
	if r == nil || r.release == nil {
		return
	}
	if !force && r.pending.Load() < nativeParsePressureThresholdBytes {
		return
	}
	if !r.sweeping.CompareAndSwap(false, true) {
		return
	}
	defer r.sweeping.Store(false)

	pending := r.pending.Swap(0)
	if pending <= 0 {
		return
	}
	if !force && pending < nativeParsePressureThresholdBytes {
		// Another sweeper consumed the threshold before this goroutine won
		// the gate. Preserve the tail for the next parse-boundary sweep.
		r.pending.Add(pending)
		return
	}

	started := time.Now()
	released := r.release()
	r.elapsed.Add(int64(time.Since(started)))
	r.calls.Add(1)
	r.released.Add(uint64(released))
}

func (r *nativeParsePressureRelief) stats() nativeParsePressureStats {
	if r == nil {
		return nativeParsePressureStats{}
	}
	return nativeParsePressureStats{
		calls:         r.calls.Load(),
		releasedBytes: r.released.Load(),
		elapsed:       time.Duration(r.elapsed.Load()),
	}
}
