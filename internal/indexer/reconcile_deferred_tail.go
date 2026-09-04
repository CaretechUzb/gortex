package indexer

import (
	"context"
	"path/filepath"
	"sort"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// A reconcile's scoped tail, deferred and replayed.
//
// ReconcileRepoCtx normally finishes a scoped reconcile the way an ordinary
// file save does: resolve the mutation frontier, then run the incremental
// derived passes over exactly the files it re-indexed. Inside a batch both are
// suppressed — the batch owns the gates they need and runs its own
// workspace-wide passes at the transition — so the tail simply does not happen
// and the result comes back with DerivedTailRan false.
//
// For the warmup's own reconciles that is right: the batch's global passes ARE
// their derivation. For a repository tracked INTO the batch from outside it is
// not, and the worktree-copy path is where that bit: a diverged copy whose tail
// was skipped fell back to scheduleWorkspaceRederive, a repo-wide pass that
// re-derives edges the copy already carried. Measured 2026-09-02 on a 192-file
// divergence: 3,255s, against the 27.8s the reconcile itself took, and against
// the ~2 minutes four sibling copy-tracks needed the same day when their tails
// were allowed to run.
//
// So the tail is kept rather than discarded, and replayed when the batch ends.
// It is the same work, at the same scope, a few minutes later.

// deferredReconcileTail is one reconcile's suppressed tail, held whole.
//
// changed and deleted are the reconcile's own census, kept because
// seedDerivedFrontierFromCensus is part of the tail rather than of the
// reconcile: an empty reindex plan over real stale work would otherwise
// escalate the replayed resolve to a whole-store ResolveAll and no-op the
// derived half — the exact failure the census seeding exists to prevent.
type deferredReconcileTail struct {
	prefix  string
	idx     *Indexer
	result  *IndexResult
	receipt *graph.MutationReceipt
	batch   *reparsePendingEnrichmentBatch
	changed []string
	deleted []string
}

// deferCopiedReconcileTail records a copy-tracked repository's suppressed tail
// for replay at the batch transition. Overwrites any earlier record for the
// same prefix: a second copy-track of one prefix has re-reconciled the same
// repository, so the older tail describes a mutation the newer one has already
// superseded.
func (mi *MultiIndexer) deferCopiedReconcileTail(prefix string, tail *deferredReconcileTail) {
	if mi == nil || prefix == "" || tail == nil {
		return
	}
	mi.mu.Lock()
	defer mi.mu.Unlock()
	if mi.deferredCopyTails == nil {
		mi.deferredCopyTails = map[string]*deferredReconcileTail{}
	}
	mi.deferredCopyTails[prefix] = tail
}

// takeDeferredCopiedReconcileTails detaches every recorded tail. Destructive:
// whoever takes them owns replaying them, and a tail left in the map after a
// batch transition would be replayed again at the next one, over a receipt
// describing a mutation that is by then several generations old.
func (mi *MultiIndexer) takeDeferredCopiedReconcileTails() []*deferredReconcileTail {
	if mi == nil {
		return nil
	}
	mi.mu.Lock()
	tails := mi.deferredCopyTails
	mi.deferredCopyTails = nil
	mi.mu.Unlock()
	if len(tails) == 0 {
		return nil
	}
	prefixes := make([]string, 0, len(tails))
	for prefix := range tails {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)
	out := make([]*deferredReconcileTail, 0, len(prefixes))
	for _, prefix := range prefixes {
		out = append(out, tails[prefix])
	}
	return out
}

// replayDeferredReconcileTail runs the suppressed tail: the same
// resolve-then-derive pair ReconcileRepoCtx would have run inline, at the same
// frontier. Reports whether the derived half covered a non-empty frontier,
// which is what copiedDivergenceRepaired asks of an inline tail and therefore
// what a caller may treat as "the divergence is repaired".
//
// Takes its own gates through the public RunIncrementalDerivedPasses rather
// than the topology-held variant: it runs AFTER the batch transition, so
// nothing it needs is already held, and borrowing the inline path's assumption
// that topology is owned would deadlock.
func (mi *MultiIndexer) replayDeferredReconcileTail(ctx context.Context, tail *deferredReconcileTail) bool {
	if mi == nil || tail == nil || tail.result == nil || tail.idx == nil {
		return false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if seedDerivedFrontierFromCensus(tail.result, tail.changed, tail.deleted, func(relPath string) string {
		return tail.idx.prefixPath(filepath.FromSlash(relPath))
	}) && mi.logger != nil {
		mi.logger.Info("daemon: deferred reconcile tail seeded derived frontier from census",
			zap.String("prefix", tail.prefix),
			zap.Int("files", len(tail.result.DerivedInvalidation.Files)))
	}
	mi.resolveIncrementalRepoMutation(tail.prefix, tail.result, tail.receipt, tail.batch)
	tail.idx.observeIncrementalCatchup("derived", tail.result.DerivedInvalidation.Files)
	mi.RunIncrementalDerivedPasses(ctx, map[string]DerivedInvalidationPlan{
		tail.prefix: tail.result.DerivedInvalidation,
	})
	tail.result.DerivedTailRan = true
	return len(tail.result.DerivedInvalidation.Files) > 0
}

// flushDeferredCopiedReconcileTails replays every recorded tail and returns the
// prefixes whose divergence is now repaired, so the caller can drop them from
// the owed set instead of scheduling the repo-wide pass they were holding a
// fallback for.
//
// A tail whose replay covered nothing is NOT reported repaired. That is the
// same conservatism copiedDivergenceRepaired applies inline: an empty frontier
// despite real stale work means nothing was re-derived, and restamping over it
// would bless a silently under-derived graph.
func (mi *MultiIndexer) flushDeferredCopiedReconcileTails(ctx context.Context) []string {
	tails := mi.takeDeferredCopiedReconcileTails()
	if len(tails) == 0 {
		return nil
	}
	var repaired []string
	for _, tail := range tails {
		if !mi.replayDeferredReconcileTail(ctx, tail) {
			if mi.logger != nil {
				mi.logger.Info("worktree copy: deferred reconcile tail covered nothing; repo-wide derivation still owed",
					zap.String("repo", tail.prefix))
			}
			// The whole-repo arm the copy path already raised stands; the
			// fallback derivation the caller is about to schedule is what this
			// pass waits behind.
			mi.scheduleCopiedRepoEnrich(tail.prefix, nil)
			continue
		}
		if restamper, ok := mi.graph.(graph.CopiedReadinessRestamper); ok {
			if err := restamper.RestampCopiedReadiness(tail.prefix); err != nil && mi.logger != nil {
				mi.logger.Warn("worktree copy: could not declare repaired stage stamps current",
					zap.String("repo", tail.prefix), zap.Error(err))
			}
		}
		if mi.logger != nil {
			mi.logger.Info("worktree copy: divergence repaired by the deferred reconcile tail; no workspace rederive owed",
				zap.String("repo", tail.prefix),
				zap.Int("files", len(tail.result.DerivedInvalidation.Files)))
		}
		// Run the pass the copy path armed. The frontier is NOT passed: that arm
		// was whole-repo and markPendingEnrichFull cannot be narrowed afterwards
		// ("full work dominates any queued file frontier" — see
		// markPendingEnrichFiles), so handing a frontier here would read as a
		// narrowing that does not happen.
		//
		// Whole-repo is the deliberate trade. Arming at copy time rather than
		// here is what closes the window where a daemon that dies before the
		// transition leaves a diverged copy carrying the SOURCE's enrichment
		// rows with nothing armed to correct them — the failure this arming was
		// added for. Narrowing it would mean arming late, and a whole-repo pass
		// costs minutes against a derivation that used to cost an hour.
		mi.scheduleCopiedRepoEnrich(tail.prefix, nil)
		repaired = append(repaired, tail.prefix)
	}
	return repaired
}
