package indexer

import (
	"context"
	"slices"
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
	// inflight is the frontier the RUNNING pass took out of pending. It is
	// tracked separately only so the owed set stays whole across the hand-off:
	// a prefix moved out of pending is still owed a derivation until the pass
	// that took it returns, and publishing pending alone would blink it out of
	// the marker for the whole run.
	inflight map[string]struct{}
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

// owedLocked renders every repository a derivation is owed to but has not
// been opened for: queued in pending, taken by a pass that has not reached
// DeriveBegan yet, or deferred behind a batch. s.mu must be held.
//
// All three populations read "never derived" without it, because that verdict
// is reached by the absence of a derive_state row and none of these repos has
// one yet. The union is what makes the marker agree with
// WorkspaceRederivePending instead of covering only part of what it reports.
func (s *workspaceRederiveScheduler) owedLocked() []string {
	var out []string
	for _, set := range []map[string]struct{}{s.pending, s.inflight, s.deferred} {
		for prefix := range set {
			out = append(out, prefix)
		}
	}
	sort.Strings(out)
	return slices.Compact(out)
}

// publishOwedLocked pushes the owed set to the runtime marker. s.mu must be
// held and marker must have been resolved BEFORE it was taken: runtimeMarkerRef
// reads the indexer's own lock, and taking that underneath the scheduler's
// would introduce the only nesting between the two.
//
// The write itself happens under s.mu so publications cannot be reordered
// against the transitions that caused them. The lock guards a handful of map
// operations and never a pass, so a small file write inside it costs nothing a
// caller can observe.
func (s *workspaceRederiveScheduler) publishOwedLocked(marker RuntimeMarker) {
	if marker == nil {
		return
	}
	marker.DerivePendingChanged(s.owedLocked())
}

// scheduleWorkspaceRederive queues one workspace-wide derivation pass for
// a repository indexed outside a batch. It returns immediately; reason
// names the repo prefix that triggered it, for the log breadcrumb.
func (mi *MultiIndexer) scheduleWorkspaceRederive(reason string) {
	if mi == nil || mi.graph == nil {
		return
	}
	s := &mi.rederive
	marker := mi.runtimeMarkerRef()

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
		s.publishOwedLocked(marker)
		s.mu.Unlock()
		return
	}
	s.running = true
	debounce := s.debounce
	s.wg.Add(1)
	s.publishOwedLocked(marker)
	s.mu.Unlock()

	go func() {
		defer s.wg.Done()
		for {
			if debounce > 0 {
				time.Sleep(debounce)
			}

			marker := mi.runtimeMarkerRef()
			ctx, cancel := context.WithCancel(context.Background())
			s.mu.Lock()
			if s.closed {
				s.running = false
				// Publish empty rather than the owed set. Nothing will
				// ever run these now, so reporting them as pending would
				// strand a reader on a promise the scheduler has retired.
				if marker != nil {
					marker.DerivePendingChanged(nil)
				}
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
			// The frontier leaves pending here but is not derived yet —
			// DeriveBegan is minutes away, behind the checkout-group
			// republish, the cross-repo resolve and the batch-mutation
			// gate. Holding it in inflight is what keeps it in the owed
			// set across that whole gap.
			s.inflight = frontier
			s.cancel = cancel
			s.publishOwedLocked(marker)
			s.mu.Unlock()

			mi.runWorkspaceRederive(ctx, frontier)

			// Read the preemption verdict BEFORE cancel(). cancel() is this
			// pass's own context release, so it makes ctx.Err() non-nil
			// unconditionally — testing it afterwards classified every
			// COMPLETED pass as preempted and pushed the frontier it had just
			// finished deriving back into pending. Nothing drains that: the run
			// which would have is the one that just returned, and s.queued is
			// false on a clean finish. The repo therefore stayed in the owed set
			// and read "deriving…" until the daemon exited, hours after its
			// derive_state row went current — while runWorkspaceRederive's own
			// log line, which reads ctx.Err() before this cancel, correctly said
			// preempted=false. Those two disagreeing is the signature.
			preempted := ctx.Err() != nil

			s.mu.Lock()
			s.cancel = nil
			s.inflight = nil
			cancel()
			if preempted {
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
				s.publishOwedLocked(marker)
				s.mu.Unlock()
				continue
			}
			s.running = false
			s.publishOwedLocked(marker)
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
	marker := mi.runtimeMarkerRef()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.deferred == nil {
		s.deferred = map[string]struct{}{}
	}
	s.deferred[prefix] = struct{}{}
	// A deferred repo is owed a derivation for the whole life of the batch
	// that suppressed it, and has no derive_state row for any of it. Leaving
	// it out of the marker is what let a repo tracked during warmup read
	// "never derived" while the daemon was, correctly, holding it for EndBatch.
	s.publishOwedLocked(marker)
}

// ClearDeferredWorkspaceRederive discards the deferred set. EndBatch calls
// it because its own global pass is the derivation those repositories were
// waiting for; scheduling another would double every cold index.
func (mi *MultiIndexer) ClearDeferredWorkspaceRederive() {
	if mi == nil {
		return
	}
	s := &mi.rederive
	marker := mi.runtimeMarkerRef()
	s.mu.Lock()
	s.deferred = nil
	s.publishOwedLocked(marker)
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
	// Replay first, schedule second. A copy-tracked repository whose scoped
	// tail the batch suppressed is owed exactly that tail, not a workspace-wide
	// pass, and replaying it here is what lets the prefix leave the deferred set
	// below instead of paying one. Runs unconditionally: EndBatch clears the
	// deferred set on its way out, so by the time this is called the set can be
	// empty while a recorded tail is still outstanding.
	repaired := mi.flushDeferredCopiedReconcileTails(context.Background())
	s.mu.Lock()
	deferred := s.deferred
	s.deferred = nil
	for _, prefix := range repaired {
		delete(deferred, prefix)
	}
	s.mu.Unlock()
	if len(deferred) == 0 {
		// The marker still has to be retired: a prefix whose tail replayed left
		// the deferred set here and nothing else would publish its departure.
		if len(repaired) > 0 {
			marker := mi.runtimeMarkerRef()
			s.mu.Lock()
			s.publishOwedLocked(marker)
			s.mu.Unlock()
		}
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
	// Each schedule republishes, so the marker is already right in the normal
	// case. This covers the one path that does not: scheduleWorkspaceRederive
	// returns without publishing when there is no graph, and these prefixes
	// have just left the deferred set, so nothing else would retire them.
	marker := mi.runtimeMarkerRef()
	s.mu.Lock()
	s.publishOwedLocked(marker)
	s.mu.Unlock()
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
	marker := mi.runtimeMarkerRef()
	s.mu.Lock()
	s.closed = true
	s.queued = false
	s.pending = nil
	s.deferred = nil
	// inflight is deliberately left alone: the pass holding it is still
	// unwinding, and its own exit publishes. Retiring the set here covers the
	// case where no pass was running at all, which has no other publisher.
	s.publishOwedLocked(marker)
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

	var covered []string
	var passErr error
	if ctx.Err() == nil {
		mi.batchMutationGate.Lock()
		covered, passErr = mi.runGlobalGraphPasses(ctx, scope, false)
		mi.batchMutationGate.Unlock()
	}
	// A preempted run leaves passErr non-nil and stamps nothing, so the repos
	// keep reading partial until the scheduler's re-run completes. The gate
	// above is held for the whole pass, which is what lets the stamp record the
	// generation the passes themselves left the graph at.
	mi.completeDerive(covered, scope != nil, passErr)

	if mi.logger != nil {
		mi.logger.Info("workspace derivation complete (post-track)",
			zap.String("triggered_by", reason),
			zap.Bool("scoped", scope != nil),
			zap.Bool("preempted", ctx.Err() != nil),
			zap.Int("derived_repos", len(covered)),
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
