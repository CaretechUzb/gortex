package indexer

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Workspace re-derivation after an out-of-batch repository index.
//
// TrackRepoCtx indexes one repository and installs it. Everything derived
// from the REST of the workspace INTO that repository — framework-dispatch
// synthesis, cross-repo edges, inferred implements/overrides, test and
// capability edges — is produced by the workspace-wide global passes, and
// those run only at EndBatch. A track outside a batch therefore left the
// graph under-bound, and silently: measured on a five-repo Odoo workspace,
// an untrack + track of `odoo` took its edge count from 912,448 to
// 402,411, and no later daemon restart recovered it, because a warm
// restart sees a repo whose nodes are already on disk and takes the
// ResetBatch fast path that skips the passes entirely.
//
// The frontier has to be the whole workspace, not the tracked repository.
// A global pass owns an edge by its SOURCE node, so the bindings lost when
// `odoo` is retracked are precisely the ones whose source lives in
// `addons` or `local` — the repos a repo-scoped frontier excludes. Passing
// a nil scope is both correct and cheaper than naming every prefix: the
// graph scans then select whole-store partial indexes instead of
// materialising an all-repository join table.
//
// The pass costs minutes on a large workspace, so it runs in the
// background. The daemon holds its control lock across Track, and blocking
// that for the duration would stall every concurrent status / untrack
// call. At most one pass runs and at most one waits behind it, so a burst
// of tracks (`daemon reload` adopting several repos at once) collapses
// into a bounded number of passes rather than one full pass per repo.
//
// # Preemption
//
// Running in the background is not enough on its own. The pass takes the
// batch-transition write side and the reachability topology writer for its
// whole duration, exactly as EndBatch does, because a repository must not
// appear or vanish underneath a half-finished derivation. Both of those
// gates are what an untrack, a track, and a batch transition need, so a
// pass that merely runs "in the background" still stops every one of them
// dead — measured on a two-repo workspace, an untrack issued during a pass
// took 19.7 minutes and `daemon stop` had to force-kill.
//
// So the pass is preemptible instead of merely asynchronous.
// preemptWorkspaceRederive cancels the running pass's context and re-arms
// the queue; runGlobalGraphPassesTopologyHeld checks that context at every
// sub-pass boundary and returns, releasing both gates; the scheduler then
// runs the pass again once the mutation that preempted it has landed. The
// derivation is idempotent and always re-runs from the whole workspace, so
// an abandoned pass costs time, never correctness — whereas the
// half-derived graph it used to leave behind was permanent.

// postTrackRederiveDebounce delays the first pass so a burst of tracks
// coalesces into one. Only NewMultiIndexer installs it; a zero value —
// what a bare struct literal in a test carries — runs immediately.
const postTrackRederiveDebounce = 2 * time.Second

// workspaceRederiveScheduler serialises workspace-wide derivation passes
// into a run slot plus a single queued slot.
type workspaceRederiveScheduler struct {
	mu      sync.Mutex
	wg      sync.WaitGroup
	running bool
	queued  bool
	// closed rejects new passes after Close. A pass in flight when Close
	// lands is cancelled rather than awaited to completion.
	closed bool
	// cancel stops the pass currently executing; nil whenever no pass is
	// between its context's creation and its return.
	cancel   context.CancelFunc
	debounce time.Duration
}

// scheduleWorkspaceRederive queues one workspace-wide derivation pass for
// a repository indexed outside a batch. It returns immediately; reason
// names the repo prefix that triggered it, for the log breadcrumb.
func (mi *MultiIndexer) scheduleWorkspaceRederive(reason string) {
	if mi == nil || mi.graph == nil {
		return
	}
	s := &mi.rederive

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if s.running {
		// A pass is already in flight. It cannot be reused — it may
		// have read past this repository's nodes already — so mark
		// the queue and let the running goroutine loop once more.
		s.queued = true
		s.mu.Unlock()
		return
	}
	s.running = true
	debounce := s.debounce
	s.wg.Add(1)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		for {
			if debounce > 0 {
				time.Sleep(debounce)
			}

			ctx, cancel := context.WithCancel(context.Background())
			s.mu.Lock()
			if s.closed {
				s.running = false
				s.mu.Unlock()
				cancel()
				return
			}
			// Clear the queue flag BEFORE the pass, not after: a
			// track that lands mid-pass must set it again and earn
			// another run, since this one may already be past its
			// repository.
			s.queued = false
			s.cancel = cancel
			s.mu.Unlock()

			mi.runWorkspaceRederive(ctx, reason)

			s.mu.Lock()
			s.cancel = nil
			cancel()
			if s.queued && !s.closed {
				s.mu.Unlock()
				continue
			}
			s.running = false
			s.mu.Unlock()
			return
		}
	}()
}

// preemptWorkspaceRederive asks an in-flight derivation to abandon its
// remaining passes and re-queues it to run again afterwards. Callers are
// the repository topology mutations and batch transitions that need the
// gates the pass holds; they must call this BEFORE taking any of those
// gates, so the pass can reach its next boundary and release them.
//
// It never blocks: the caller's own gate acquisition is what waits, and it
// waits only as long as the current sub-pass, not the whole derivation.
// Returns whether a pass was actually asked to stop.
func (mi *MultiIndexer) preemptWorkspaceRederive() bool {
	if mi == nil {
		return false
	}
	s := &mi.rederive
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.closed {
		return false
	}
	// Re-queue before cancelling. The work this pass is abandoning is
	// still owed to the graph, and the mutation about to run only adds
	// to it.
	s.queued = true
	if s.cancel != nil {
		s.cancel()
	}
	return true
}

// stopWorkspaceRederive closes the scheduler and waits for any in-flight
// pass to unwind. Unlike WaitWorkspaceRederive it does not wait for the
// derivation to FINISH — it cancels it, so teardown is bounded by one
// sub-pass rather than by a multi-minute workspace derivation.
func (mi *MultiIndexer) stopWorkspaceRederive() {
	if mi == nil {
		return
	}
	s := &mi.rederive
	s.mu.Lock()
	s.closed = true
	s.queued = false
	if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// runWorkspaceRederive runs the global passes over the whole workspace,
// holding the batch-transition gate for the duration exactly as EndBatch
// does, so a batch cannot open underneath a running pass. ctx is the
// preemption channel — see the Preemption note at the top of this file.
func (mi *MultiIndexer) runWorkspaceRederive(ctx context.Context, reason string) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	start := time.Now()
	if mi.logger != nil {
		mi.logger.Info("workspace derivation starting (post-track)",
			zap.String("triggered_by", reason))
	}

	// Cross-repo resolve first, then the derivation passes — the order the
	// daemon warmup uses. Resolve lifts the references that now span a
	// repo boundary (a call the tracked repo answers, or one it makes into
	// a sibling); the passes below derive from what resolve produced, so
	// running them first would derive from a half-bound graph.
	mi.RunGlobalResolve()

	if ctx.Err() == nil {
		mi.batchMutationGate.Lock()
		mi.runGlobalGraphPasses(ctx, nil, false)
		mi.batchMutationGate.Unlock()
	}

	if mi.logger != nil {
		mi.logger.Info("workspace derivation complete (post-track)",
			zap.String("triggered_by", reason),
			zap.Bool("preempted", ctx.Err() != nil),
			zap.Duration("elapsed", time.Since(start)))
	}
}

// WorkspaceRederivePending reports whether a post-track workspace
// derivation is running or queued. Derived edges into a repository
// tracked since the last pass are incomplete until this goes false.
func (mi *MultiIndexer) WorkspaceRederivePending() bool {
	if mi == nil {
		return false
	}
	mi.rederive.mu.Lock()
	defer mi.rederive.mu.Unlock()
	return mi.rederive.running
}

// WaitWorkspaceRederive blocks until no post-track derivation is running.
// A pass queued after the wait begins is not awaited.
func (mi *MultiIndexer) WaitWorkspaceRederive() {
	if mi == nil {
		return
	}
	mi.rederive.wg.Wait()
}
