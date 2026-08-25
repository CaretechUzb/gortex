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

// postTrackRederiveDebounce delays the first pass so a burst of tracks
// coalesces into one. Only NewMultiIndexer installs it; a zero value —
// what a bare struct literal in a test carries — runs immediately.
const postTrackRederiveDebounce = 2 * time.Second

// workspaceRederiveScheduler serialises workspace-wide derivation passes
// into a run slot plus a single queued slot.
type workspaceRederiveScheduler struct {
	mu       sync.Mutex
	wg       sync.WaitGroup
	running  bool
	queued   bool
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
			// Clear the queue flag BEFORE the pass, not after: a
			// track that lands mid-pass must set it again and earn
			// another run, since this one may already be past its
			// repository.
			s.mu.Lock()
			s.queued = false
			s.mu.Unlock()

			mi.runWorkspaceRederive(reason)

			s.mu.Lock()
			if !s.queued {
				s.running = false
				s.mu.Unlock()
				return
			}
			s.mu.Unlock()
		}
	}()
}

// runWorkspaceRederive runs the global passes over the whole workspace,
// holding the batch-transition gate for the duration exactly as EndBatch
// does, so a batch cannot open underneath a running pass.
func (mi *MultiIndexer) runWorkspaceRederive(reason string) {
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

	mi.batchMutationGate.Lock()
	// context.Background, not the tracking caller's context: the RPC that
	// triggered this has already returned, and a pass abandoned halfway
	// leaves exactly the half-derived graph this fixes.
	mi.runGlobalGraphPasses(context.Background(), nil, false)
	mi.batchMutationGate.Unlock()

	if mi.logger != nil {
		mi.logger.Info("workspace derivation complete (post-track)",
			zap.String("triggered_by", reason),
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
