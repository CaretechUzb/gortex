package indexer

import (
	"context"
	"errors"
	"fmt"
	"uuid"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/gitstate"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/reconcile"
)

// ErrCheckoutMoved reports that a checkout changed state under an operation
// that had to describe one state of it. The corpus a promotion built would
// describe a working tree the checkout has already left, so it is discarded
// rather than published against an identity it does not match.
var ErrCheckoutMoved = errors.New("indexer: the checkout moved under the operation")

// PromoteResult is what one promotion did.
type PromoteResult struct {
	// CheckoutID is the identity that was promoted. It does not change: a
	// promotion moves where a checkout is served from, not who it is.
	CheckoutID string
	// Prefix is the repo prefix the new dedicated corpus lives under.
	Prefix string
	// GraphID is the dedicated graph bound to that prefix.
	GraphID string
	// Index is the full index that built the corpus, nil when the corpus was
	// already there.
	Index *IndexResult
	// Resampled counts the times the checkout moved under the index and the
	// build was taken again.
	Resampled int
	// Retryable reports that the journalled intent is still standing, so the
	// same call can be made again. It is only ever true alongside an error.
	Retryable bool
}

// PromoteCheckout gives an automatic checkout a corpus of its own.
//
// The order is what makes it safe to interrupt. The intent is journalled
// first, so a promotion that dies anywhere leaves a durable record of what was
// being asked for. Then the checkout is sampled, indexed into a NEW prefix as
// its own base corpus, and sampled again: a working tree that moved under the
// index produced a corpus describing a state the checkout has left, and the
// build is taken again rather than published. Only once a corpus that matches
// the checkout exists does anything the query surface reads change — the mode
// flips to dedicated and the automatic route and coordinator are retired.
//
// Every failure before that flip leaves the automatic view serving. The corpus
// half-built for the new prefix is evicted, the graph row that named it is
// dropped, and the journalled intent stays pending, which is what Retryable
// means: nothing about the checkout changed, so asking again is the whole of
// the recovery.
func (l *CheckoutLifecycle) PromoteCheckout(
	ctx context.Context, checkoutID string, source store_sqlite.IntentSourceKind,
) (PromoteResult, error) {
	if l == nil || l.mi == nil {
		return PromoteResult{}, errors.New("indexer: checkout lifecycle is not wired")
	}
	if l.catalog == nil {
		return PromoteResult{}, errNoCatalog
	}
	checkout, err := l.checkoutStateOf(ctx, checkoutID)
	if err != nil {
		return PromoteResult{}, err
	}
	out := PromoteResult{CheckoutID: checkoutID}
	if checkout.EffectiveMode == store_sqlite.CheckoutModeDedicated {
		out.Prefix = l.prefixForCheckout(ctx, checkoutID)
		out.GraphID = GraphIDFor(out.Prefix)
		return out, nil
	}
	if checkout.State != store_sqlite.CheckoutStateReady {
		return out, fmt.Errorf("%w: checkout %s is %s, not ready",
			ErrCheckoutMoved, checkoutID, checkout.State)
	}
	defer l.beginBatch()()

	transition, err := l.beginModeChange(ctx, checkout, store_sqlite.CheckoutModeDedicated, "promote_checkout")
	if err != nil {
		return out, err
	}
	if source != TrackSourceImplicit {
		intent := store_sqlite.TrackingIntent{
			IntentID:      uuid.NewV7().String(),
			CheckoutID:    checkoutID,
			SourceKind:    source,
			SourceLocator: checkout.RootPath,
			Active:        true,
			CreatedAt:     l.now().Unix(),
		}
		if err := l.catalog.UpsertTrackingIntent(ctx, intent); err != nil {
			return out, l.promotionFailed(ctx, &out, transition, err)
		}
	}

	out.Prefix = l.dedicatedPrefixFor(ctx, checkout.RootPath)
	if out.Prefix == "" {
		return out, l.promotionFailed(ctx, &out, transition,
			fmt.Errorf("indexer: no dedicated prefix can be derived for %s", checkout.RootPath))
	}

	index, resampled, err := l.indexPromotedCorpus(ctx, checkout, out.Prefix)
	out.Index, out.Resampled = index, resampled
	if err != nil {
		l.rollbackPromotion(ctx, out.Prefix, "")
		return out, l.promotionFailed(ctx, &out, transition, err)
	}

	out.GraphID = GraphIDFor(out.Prefix)
	row := store_sqlite.DedicatedGraph{
		GraphID:         out.GraphID,
		OwnerCheckoutID: checkoutID,
		RepoPrefix:      out.Prefix,
		FamilyID:        checkout.FamilyID,
		// Never the primary. A promotion is a worktree asking for a corpus of
		// its own, not for the family's base to move to it — that is what
		// SetPrimary is, and it carries the family's epoch guard.
		IsPrimaryBase: false,
		State:         reconcile.GraphStateReady,
	}
	if err := l.catalog.UpsertDedicatedGraph(ctx, row); err != nil {
		l.rollbackPromotion(ctx, out.Prefix, "")
		return out, l.promotionFailed(ctx, &out, transition, err)
	}

	if err := l.serveFromOwnCorpus(ctx, checkout); err != nil {
		l.rollbackPromotion(ctx, out.Prefix, out.GraphID)
		return out, l.promotionFailed(ctx, &out, transition, err)
	}

	l.attachWatcher(out.Prefix)
	l.saveConfig("promote")
	l.notifyTrackedSetChanged()
	if err := l.catalog.CompleteIntentTransition(ctx, checkoutID, transition.TransitionID); err != nil {
		// The promotion happened; only the journal slot is still occupied. The
		// next pass over the checkout releases it, and reporting a failure here
		// would invite a retry of work that is already done.
		l.logger.Warn("checkout lifecycle: could not release the promotion journal",
			zap.String("checkout", checkoutID), zap.Error(err))
	}
	return out, nil
}

// checkoutSample is the state a promotion has to describe: what the checkout
// is committed at, and what its working tree holds on top.
type checkoutSample struct {
	tree        string
	commit      string
	fingerprint string
}

// sampleCheckout reads both halves of a checkout's state in one go.
func sampleCheckout(ctx context.Context, root string) (checkoutSample, error) {
	head, err := gitstate.SampleHEAD(ctx, root)
	if err != nil {
		return checkoutSample{}, fmt.Errorf("indexer: sample HEAD of %s: %w", root, err)
	}
	dirty, err := gitstate.SampleDirty(ctx, root)
	if err != nil {
		return checkoutSample{}, fmt.Errorf("indexer: sample %s: %w", root, err)
	}
	return checkoutSample{tree: head.TreeOID, commit: head.CommitOID, fingerprint: dirty.Fingerprint}, nil
}

// indexPromotedCorpus builds the checkout's own base corpus, and insists that
// the corpus describes the checkout as it was when the index finished.
//
// A full index of a working tree takes as long as it takes, and a checkout
// under an agent's hands can commit or save part way through it. The result
// would be a corpus that is a mixture of two states, published as generation 0
// of a prefix and never diffed against anything — nothing downstream could
// ever notice. So the state is sampled on both sides of the index and the
// build is taken again when it moved.
//
// Twice, and no more. A checkout that will not hold still for one index will
// not hold still for a third, and the promotion has to fail while the
// automatic view is still there to fall back on.
func (l *CheckoutLifecycle) indexPromotedCorpus(
	ctx context.Context, checkout store_sqlite.Checkout, prefix string,
) (*IndexResult, int, error) {
	resampled := 0
	for attempt := 0; attempt < 2; attempt++ {
		before, err := sampleCheckout(ctx, checkout.RootPath)
		if err != nil {
			return nil, resampled, err
		}
		if l.indexBarrier != nil {
			l.indexBarrier()
		}
		result, err := l.mi.TrackRepoCtx(ctx, config.RepoEntry{Path: checkout.RootPath, Name: prefix})
		if err != nil {
			return nil, resampled, err
		}
		if result == nil && l.mi.GetMetadata(prefix) == nil {
			return nil, resampled, fmt.Errorf(
				"indexer: %s could not be indexed under prefix %s", checkout.RootPath, prefix)
		}
		after, err := sampleCheckout(ctx, checkout.RootPath)
		if err != nil {
			return nil, resampled, err
		}
		if after == before {
			return result, resampled, nil
		}
		resampled++
		l.mi.UntrackRepo(prefix)
	}
	return nil, resampled, fmt.Errorf("%w: %s moved under two full indexes",
		ErrCheckoutMoved, checkout.RootPath)
}

// serveFromOwnCorpus is the flip: the checkout becomes dedicated and stops
// being served through the family's automatic lane.
//
// The coordinator is stopped first, because it is the one thing that would
// rebuild the route this is about to withdraw. Then the mode moves under the
// incarnation guard — it is what every other surface reads to decide which
// lane a checkout is in — and only then does the route come down.
//
// A flip that loses its guard leaves the route and both of its generations
// exactly where they were, so the automatic view keeps serving; what it loses
// is the coordinator, which the next sweep brings back for a checkout the
// catalog still calls automatic.
//
// The mode flip is the commit point. Everything after it is cleanup, and is
// reported as such: a dedicated checkout is read from its own corpus whatever
// the route says, so a withdrawal that fails leaves a row nothing consults —
// not a promotion to undo. Returning the failure would run the rollback over a
// corpus every reader has already been pointed at, and would journal a retry
// that finds the checkout dedicated and does nothing.
func (l *CheckoutLifecycle) serveFromOwnCorpus(ctx context.Context, checkout store_sqlite.Checkout) error {
	l.dropCoordinator(checkout.CheckoutID)
	err := l.catalog.UpdateCheckoutState(ctx, store_sqlite.UpdateCheckoutStateRequest{
		CheckoutID:    checkout.CheckoutID,
		Incarnation:   checkout.Incarnation,
		State:         store_sqlite.CheckoutStateReady,
		DesiredMode:   store_sqlite.CheckoutModeDedicated,
		EffectiveMode: store_sqlite.CheckoutModeDedicated,
		LastSeen:      l.now().Unix(),
	})
	if err != nil {
		return err
	}
	// Read before the withdrawal: the route row is the only thing in the
	// catalog that names a checkout's two generations, so once it is gone the
	// payload has no id anything could offer for collection.
	l.oweRoutedGenerations(ctx, checkout.CheckoutID)
	l.withdrawAutomaticRoute(ctx, checkout.CheckoutID)
	l.sweepRetirements(ctx)
	return nil
}

// withdrawAutomaticRoute takes down the route a checkout was served through in
// the family's automatic lane.
//
// A failure is logged and left standing rather than reported. The row it could
// not remove routes nothing — the checkout's mode has already moved — and the
// sweep withdraws it on its next pass over the family, which is also what frees
// the two generations it is still naming.
func (l *CheckoutLifecycle) withdrawAutomaticRoute(ctx context.Context, checkoutID string) {
	withdraw := l.catalog.DeleteCheckoutRoute
	if l.routeBarrier != nil {
		withdraw = l.routeBarrier
	}
	if err := withdraw(ctx, checkoutID); err != nil &&
		!errors.Is(err, store_sqlite.ErrCatalogNotFound) {
		l.logger.Warn("checkout lifecycle: could not withdraw a promoted checkout's automatic route",
			zap.String("checkout", checkoutID), zap.Error(err))
	}
}

// rollbackPromotion undoes what a failed promotion built. The automatic view
// was never touched, so putting the corpus and the graph row back the way they
// were is the whole of it.
func (l *CheckoutLifecycle) rollbackPromotion(ctx context.Context, prefix, graphID string) {
	if graphID != "" {
		if err := l.catalog.DeleteDedicatedGraph(ctx, graphID); err != nil &&
			!errors.Is(err, store_sqlite.ErrCatalogNotFound) {
			l.logger.Warn("checkout lifecycle: could not drop a rolled-back graph binding",
				zap.String("graph", graphID), zap.Error(err))
		}
	}
	if prefix != "" && l.mi.GetMetadata(prefix) != nil {
		l.evictRepo(prefix)
	}
}

// beginModeChange journals a mode change, adopting the entry an interrupted
// attempt left behind rather than refusing to start beside it.
func (l *CheckoutLifecycle) beginModeChange(
	ctx context.Context,
	checkout store_sqlite.Checkout,
	requested store_sqlite.CheckoutMode,
	cause string,
) (store_sqlite.IntentTransition, error) {
	now := l.now().Unix()
	transition := store_sqlite.IntentTransition{
		TransitionID:       uuid.NewV7().String(),
		CheckoutID:         checkout.CheckoutID,
		Cause:              cause,
		PriorDesiredMode:   checkout.DesiredMode,
		PriorEffectiveMode: checkout.EffectiveMode,
		RequestedMode:      requested,
		PriorCheckoutState: checkout.State,
		State:              store_sqlite.IntentTransitionRunning,
		CreatedAt:          now,
		LastProgress:       now,
	}
	err := l.catalog.BeginIntentTransition(ctx, transition)
	if err == nil {
		return transition, nil
	}
	if !errors.Is(err, store_sqlite.ErrCatalogIntentTransitionActive) {
		return store_sqlite.IntentTransition{}, err
	}
	standing, found, lookupErr := l.catalog.GetIntentTransition(ctx, checkout.CheckoutID)
	if lookupErr != nil {
		return store_sqlite.IntentTransition{}, lookupErr
	}
	if !found || standing.RequestedMode != requested {
		// Some other change is in flight. One slot per checkout is the rule,
		// and running two mode changes over one identity is what it exists to
		// stop.
		return store_sqlite.IntentTransition{}, err
	}
	standing.State = store_sqlite.IntentTransitionRunning
	standing.LastProgress = now
	if err := l.catalog.UpdateIntentTransitionProgress(ctx, checkout.CheckoutID,
		standing.TransitionID, store_sqlite.IntentTransitionRunning, "", now); err != nil {
		return store_sqlite.IntentTransition{}, err
	}
	return standing, nil
}

// promotionFailed leaves the journalled intent standing and pending, which is
// what makes the same call a retry rather than a second request — and says so
// on the result, since nothing about the checkout changed.
func (l *CheckoutLifecycle) promotionFailed(
	ctx context.Context, out *PromoteResult, transition store_sqlite.IntentTransition, cause error,
) error {
	err := l.catalog.UpdateIntentTransitionProgress(ctx, out.CheckoutID, transition.TransitionID,
		store_sqlite.IntentTransitionPending, cause.Error(), l.now().Unix())
	if err != nil {
		l.logger.Warn("checkout lifecycle: could not journal a failed promotion",
			zap.String("checkout", out.CheckoutID), zap.Error(err))
		return cause
	}
	out.Retryable = true
	return cause
}
