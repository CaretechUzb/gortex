package indexer

import (
	"context"
	"runtime"
	"sync"
	"time"

	"go.uber.org/zap"
)

// A fired debounce cohort used to put every changed path into patchGraph
// at once: one time.AfterFunc per path, each goroutine blocking on the
// repository mutation lane until its turn. Only one can make progress, so
// the rest are pure backlog — and Go keeps a goroutine's stack descriptor
// on the scheduler free list forever, which makes a burst's peak goroutine
// count a permanent heap cost no GC or FreeOSMemory can return.
//
// Storm batching (WatchConfig.StormThreshold) collapses a large burst into
// a single timer and is the primary defence. These bounds cover what stays
// on the per-file path: how many callbacks may be in the patch path at
// once, and how long any of them will wait before giving up.
const (
	// mutationAdmissionWaitTimeout is how long a debounce callback waits
	// for a work slot. A jammed lane sheds the patch instead of parking a
	// goroutine on it indefinitely; the reconcile janitor still picks the
	// file up on its next sweep, so the index converges either way.
	mutationAdmissionWaitTimeout = 2 * time.Minute
	// mutationLaneTimeout bounds the wait for the repository mutation lane
	// itself. Generous on purpose — a legitimate structural patch on a
	// large repo has been measured in the tens of seconds — so it only
	// fires for a lane that is genuinely stuck.
	mutationLaneTimeout = 10 * time.Minute
	// mutationShedLogInterval throttles the shed warning. A jammed lane
	// sheds one patch per queued callback, so logging each one unthrottled
	// emits a line per shed — thousands per minute on a repo with a large
	// generated tree, which buries the storm-drain and reconcile records
	// that explain why the lane is jammed in the first place. One line per
	// interval carrying the suppressed count and a sample path is what a
	// diagnosis actually needs.
	mutationShedLogInterval = 30 * time.Second
)

// mutationWorkSlots sizes the cohort semaphore. Patching is CPU- and
// allocation-heavy and serializes on the lane anyway, so there is nothing
// to gain from admitting more than the machine can run.
func mutationWorkSlots() int {
	n := runtime.GOMAXPROCS(0)
	if n < 2 {
		return 2
	}
	return n
}

// admitMutationWork takes a slot in the watcher's cohort semaphore. The
// returned release must be called when the work is done. admitted is
// false when the wait timed out or the watcher is stopping, in which case
// release is a no-op and the caller must not proceed. path names the file
// whose patch is at stake; it is only used to give the shed warning a
// sample worth acting on.
func (w *Watcher) admitMutationWork(path string) (release func(), admitted bool) {
	if w == nil {
		return func() {}, false
	}
	slots := w.mutationSlots()
	if slots == nil {
		return func() {}, true
	}
	timer := time.NewTimer(mutationAdmissionWaitTimeout)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return func() { <-slots }, true
	case <-w.done:
		return func() {}, false
	case <-timer.C:
		w.noteShedPatch(path)
		return func() {}, false
	}
}

// noteShedPatch records one shed patch and writes at most one warning per
// mutationShedLogInterval. The first shed of a burst logs immediately so the
// condition is never silent; the rest accumulate into that line's count.
func (w *Watcher) noteShedPatch(path string) {
	if w == nil || w.logger == nil {
		return
	}
	w.shedMu.Lock()
	w.shedCount++
	if w.shedSample == "" {
		w.shedSample = path
	}
	now := time.Now()
	first := w.shedWindowStart.IsZero()
	if !first && now.Sub(w.shedWindowStart) < mutationShedLogInterval {
		w.shedMu.Unlock()
		return
	}
	count := w.shedCount
	sample := w.shedSample
	window := now.Sub(w.shedWindowStart)
	w.shedWindowStart = now
	w.shedCount = 0
	w.shedSample = ""
	w.shedMu.Unlock()

	fields := []zap.Field{
		zap.Int("shed", count),
		zap.Duration("waited", mutationAdmissionWaitTimeout),
		zap.String("sample_path", sample),
	}
	if !first {
		fields = append(fields, zap.Duration("window", window))
	}
	w.logger.Warn("watcher: mutation admission timed out; shedding patches", fields...)
}

// mutationSlots returns the cohort semaphore, creating it on first use so
// a Watcher built by a test without the constructor still gets one.
func (w *Watcher) mutationSlots() chan struct{} {
	w.mutationSlotsOnce.Do(func() {
		w.mutationSlotsCh = make(chan struct{}, mutationWorkSlots())
	})
	return w.mutationSlotsCh
}

// watcherMutationAdmission is embedded in Watcher.
type watcherMutationAdmission struct {
	mutationSlotsOnce sync.Once
	mutationSlotsCh   chan struct{}

	// shed* throttle the admission-timeout warning. Guarded by shedMu
	// because every debounce callback that gives up races here.
	shedMu          sync.Mutex
	shedWindowStart time.Time
	shedCount       int
	shedSample      string
}

// mutationLaneContext returns the context a watcher-driven patch uses to
// wait for the repository mutation lane. It carries a deadline so a stuck
// lane sheds work rather than accumulating blocked goroutines, and it is
// cancelled when the watcher stops.
func (w *Watcher) mutationLaneContext() (context.Context, context.CancelFunc) {
	base := context.Background()
	if w != nil && w.done != nil {
		var stop context.CancelFunc
		base, stop = contextWithDoneChannel(base, w.done)
		ctx, cancel := context.WithTimeout(base, mutationLaneTimeout)
		return ctx, func() { cancel(); stop() }
	}
	return context.WithTimeout(base, mutationLaneTimeout)
}

// contextWithDoneChannel bridges a plain done channel into a context so a
// lane wait is released the moment the watcher stops.
func contextWithDoneChannel(parent context.Context, done <-chan struct{}) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stopped := make(chan struct{})
	go func() {
		select {
		case <-done:
			cancel()
		case <-stopped:
		}
	}()
	return ctx, func() {
		close(stopped)
		cancel()
	}
}
