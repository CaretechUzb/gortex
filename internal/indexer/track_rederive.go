package indexer

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
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
	// pending names every repository tracked since the running pass
	// began. It is the pass's frontier, not merely a log breadcrumb: a
	// pass whose frontier is entirely sibling checkouts of repositories
	// already tracked runs scoped (see rederiveScope). Coalescing must
	// therefore accumulate prefixes rather than keep only the first, or
	// a burst of tracks would derive one repository and silently skip
	// the rest.
	pending map[string]struct{}
	// deferred holds repositories tracked while a batch was suppressing
	// the global passes. Whoever ends that batch decides their fate: a
	// real EndBatch derives them and clears the set, while the warm
	// restart's two fast paths — nothing changed, or an exact file-level
	// delta — run no global pass at all and must hand them back. Without
	// this a repository tracked during warmup joined the graph carrying
	// only its own extraction edges, permanently: the next warm restart
	// sees its nodes already on disk and takes the same fast path again.
	deferred map[string]struct{}
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
	if s.pending == nil {
		s.pending = map[string]struct{}{}
	}
	if reason != "" {
		s.pending[reason] = struct{}{}
	}
	if s.running {
		// A pass is already in flight. It cannot be reused — it may
		// have read past this repository's nodes already — so mark
		// the queue and let the running goroutine loop once more.
		// The prefix is already in pending, so the next pass covers it.
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
			// repository. The frontier is taken on the same
			// principle — a prefix added while the pass runs belongs
			// to the NEXT pass, not this one.
			s.queued = false
			frontier := s.pending
			s.pending = map[string]struct{}{}
			s.cancel = cancel
			s.mu.Unlock()

			mi.runWorkspaceRederive(ctx, frontier)

			s.mu.Lock()
			s.cancel = nil
			cancel()
			if ctx.Err() != nil {
				// Preempted. The frontier this pass abandoned is
				// still owed to the graph, and the next pass has to
				// cover it or a scoped run would derive only
				// whatever was tracked afterwards.
				if s.pending == nil {
					s.pending = map[string]struct{}{}
				}
				for prefix := range frontier {
					s.pending[prefix] = struct{}{}
				}
			}
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

// deferWorkspaceRederive records a repository tracked inside a batch that
// is suppressing the global passes. It starts nothing — the batch owns the
// gates a derivation needs — and only remembers what is owed.
func (mi *MultiIndexer) deferWorkspaceRederive(prefix string) {
	if mi == nil || prefix == "" {
		return
	}
	s := &mi.rederive
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.deferred == nil {
		s.deferred = map[string]struct{}{}
	}
	s.deferred[prefix] = struct{}{}
}

// ClearDeferredWorkspaceRederive discards the deferred set. EndBatch calls
// it because its own global pass is the derivation those repositories were
// waiting for; scheduling another would double every cold index.
func (mi *MultiIndexer) ClearDeferredWorkspaceRederive() {
	if mi == nil {
		return
	}
	s := &mi.rederive
	s.mu.Lock()
	s.deferred = nil
	s.mu.Unlock()
}

// FlushDeferredWorkspaceRederive schedules a derivation for every
// repository tracked inside a batch that ended without running the global
// passes. Returns the prefixes it scheduled, for the caller's breadcrumb.
func (mi *MultiIndexer) FlushDeferredWorkspaceRederive() []string {
	if mi == nil {
		return nil
	}
	s := &mi.rederive
	s.mu.Lock()
	deferred := s.deferred
	s.deferred = nil
	s.mu.Unlock()
	if len(deferred) == 0 {
		return nil
	}
	prefixes := make([]string, 0, len(deferred))
	for prefix := range deferred {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		mi.scheduleWorkspaceRederive(prefix)
	}
	return prefixes
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
	s.pending = nil
	s.deferred = nil
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
func (mi *MultiIndexer) runWorkspaceRederive(ctx context.Context, frontier map[string]struct{}) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return
	}
	start := time.Now()
	// The grouping has to be current before the scope decision reads it:
	// the repository that triggered this pass was tracked since the last
	// one, so until this runs the graph does not yet know it is a
	// worktree of anything. Idempotent, and the pass republishes it.
	mi.publishCheckoutGroups()
	scope := mi.rederiveScope(frontier)
	reason := rederiveReason(frontier)
	if mi.logger != nil {
		mi.logger.Info("workspace derivation starting (post-track)",
			zap.String("triggered_by", reason),
			zap.Bool("scoped", scope != nil))
	}

	// Cross-repo resolve first, then the derivation passes — the order the
	// daemon warmup uses. Resolve lifts the references that now span a
	// repo boundary (a call the tracked repo answers, or one it makes into
	// a sibling); the passes below derive from what resolve produced, so
	// running them first would derive from a half-bound graph.
	mi.RunGlobalResolve()

	if ctx.Err() == nil {
		mi.batchMutationGate.Lock()
		mi.runGlobalGraphPasses(ctx, scope, false)
		mi.batchMutationGate.Unlock()
	}

	if mi.logger != nil {
		mi.logger.Info("workspace derivation complete (post-track)",
			zap.String("triggered_by", reason),
			zap.Bool("scoped", scope != nil),
			zap.Bool("preempted", ctx.Err() != nil),
			zap.Duration("elapsed", time.Since(start)))
	}
}

// rederiveScope decides the frontier the global passes run over.
//
// The file comment above explains why a post-track derivation is normally
// whole-workspace: a global pass owns an edge by its SOURCE node, so the
// bindings a retracked repository loses are precisely the ones sourced in
// its neighbours, and a frontier naming only the tracked repository would
// never re-derive them.
//
// A newly tracked SIBLING CHECKOUT is the one case where that argument
// does not apply, and it is the case that costs the most. The prefix has
// never existed in the graph before, so no edge pointing into it was
// dropped and there is nothing in a neighbour to re-derive. What a
// whole-store frontier does instead is re-derive every other repository
// from scratch — measured at 1,525s on a five-repo Odoo workspace, against
// an index phase of 283s — and offer, as its only new work, bindings from
// third repositories into a checkout that is a duplicate of one they
// already bind to. The checkout grouping exists to keep exactly those out
// (graph/checkout_groups.go).
//
// So: scope to the frontier when every repository in it is a sibling
// checkout of one already tracked; otherwise fall back to whole-store. The
// test is all-or-nothing on purpose — a burst that adds a worktree and an
// unrelated repository still needs the unrelated one derived properly.
func (mi *MultiIndexer) rederiveScope(frontier map[string]struct{}) map[string]struct{} {
	if len(frontier) == 0 || mi == nil || mi.graph == nil {
		return nil
	}
	// Cheap short-circuit for the overwhelmingly common workspace that
	// tracks no worktree at all: nothing can be a sibling of anything.
	if !graph.HasSiblingCheckouts(mi.graph) {
		return nil
	}
	mi.mu.RLock()
	tracked := make([]string, 0, len(mi.repos))
	for prefix := range mi.repos {
		tracked = append(tracked, prefix)
	}
	mi.mu.RUnlock()

	scope := make(map[string]struct{}, len(frontier))
	for prefix := range frontier {
		sibling := false
		for _, other := range tracked {
			if other != prefix && graph.SiblingCheckouts(mi.graph, prefix, other) {
				sibling = true
				break
			}
		}
		if !sibling {
			return nil
		}
		scope[prefix] = struct{}{}
	}
	return scope
}

// rederiveReason renders the frontier as the log breadcrumb it used to be.
func rederiveReason(frontier map[string]struct{}) string {
	if len(frontier) == 0 {
		return ""
	}
	prefixes := make([]string, 0, len(frontier))
	for prefix := range frontier {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	return strings.Join(prefixes, ",")
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
