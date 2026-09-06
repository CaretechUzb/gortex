package indexer

// PURPOSE — the asynchronous half of an untrack that demotes. StartApplyUntrack
// admits the demotion and returns; everything expensive happens afterwards in
// the lifecycle-owned transition worker. These tests pin what "afterwards"
// has to end with, because a caller polling for the end state (gortex untrack
// --wait) can only be right if the worker actually reaches it: the dedicated
// graph retired, the repository's rows and its config entry gone, the route
// pointing at the family primary, and — the one an observer can key on — the
// durable transition COMPLETED, i.e. its row deleted.
// KEYWORDS — untrack, demote, StartApplyUntrack, transition, pending, converge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestStartApplyUntrackReachesTheSameEndStateAsApplyUntrack is the contract
// the --wait poller is written against.
//
// StartApplyUntrack answers Pending as soon as the demotion is durably
// admitted — before the mode flip is even published. Every step the
// synchronous ApplyUntrack used to finish inside the request then runs in the
// transition worker, and the last of them is CompleteIntentTransition, which
// deletes the transition row. So "the row is gone" is the only observation
// that means the demotion is done; the effective mode is published first and
// is therefore true for the whole teardown behind it.
func TestStartApplyUntrackReachesTheSameEndStateAsApplyUntrack(t *testing.T) {
	f := newFamilyFixture(t, "async-demote")
	defer f.close()
	ctx := context.Background()

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)
	require.True(t, f.configLists(f.worktree), "the dedicated worktree is in the tracked set")

	preview, err := f.lc.PreviewUntrack(ctx, f.worktree)
	require.NoError(t, err)
	require.Equal(t, UntrackPlanDemote, preview.Plan)

	result, err := f.lc.StartApplyUntrack(ctx, preview)
	require.NoError(t, err)
	assert.True(t, result.Pending, "the demotion is admitted, not finished")
	assert.False(t, result.Demoted, "and must not claim to be demoted before the worker has run")
	require.NotEmpty(t, result.TransitionID)
	require.Equal(t, tracked.CheckoutID, result.CheckoutID,
		"a caller has to be given something to poll for")

	// The worker owns the lifecycle's context, not the request's; this is the
	// in-process equivalent of the CLI polling until it settles.
	f.lc.transitionWG.Wait()

	assertDemotionSettled(t, f, tracked, result.TransitionID)
	assert.True(t, f.lc.coordinatorRegistered(tracked.CheckoutID),
		"a demotion the primary can serve leaves a coordinator behind")
}

// TestDemotionFinishesEvenWhenNoCoordinatorCanBeStarted is the regression.
//
// The coordinator is a serving-side concern: it composes the automatic lane's
// layers over the primary corpus, and the automatic lane brings one up on its
// own — on the first read of the checkout, and on every family reconciliation.
// The demotion's DURABLE half is a different thing entirely, and by the time
// the coordinator is asked for, all of it but the last write has committed:
// the mode is flipped, the dedicated graph is retired, the repository's rows
// and its config entry are gone.
//
// Failing the transition there left exactly the state this whole flow exists
// to avoid — a demotion that reads as half-done to every observer (config
// entry gone, corpus gone, transition still in flight) until the hourly sweep
// resumed it, and a --wait that cannot distinguish that from a slow teardown.
// The finish now runs regardless, and the missing coordinator is a warning.
//
// The primary leaving the live index between the pre-flight and the
// coordinator is how that is provoked: it is the same race the pre-flight
// closes ahead of the first write, moved to the one window where nothing can
// be refused any more.
func TestDemotionFinishesEvenWhenNoCoordinatorCanBeStarted(t *testing.T) {
	f := newFamilyFixture(t, "no-coordinator")
	defer f.close()
	ctx := context.Background()

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	preview, err := f.lc.PreviewUntrack(ctx, f.worktree)
	require.NoError(t, err)
	require.Equal(t, UntrackPlanDemote, preview.Plan)

	// The window between the published mode flip and the coordinator that is
	// supposed to serve it. The primary's corpus leaves the live index here,
	// so buildCoordinator has nothing to compose over.
	f.lc.demoteBarrier = func() { f.mi.UntrackRepo(f.mainPrefix) }

	result, err := f.lc.StartApplyUntrack(ctx, preview)
	require.NoError(t, err)
	require.True(t, result.Pending)
	f.lc.transitionWG.Wait()

	require.False(t, f.lc.coordinatorRegistered(tracked.CheckoutID),
		"the barrier has to have actually stopped the coordinator, or this proves nothing")
	assertDemotionSettled(t, f, tracked, result.TransitionID)
}

// assertDemotionSettled is the end state a demotion converges to, whichever
// entry point started it — the same rows TestUntrackDemotesADedicatedWorktree
// asserts for the synchronous path.
func assertDemotionSettled(
	t *testing.T, f *familyFixture, tracked RegisterResult, transitionID string,
) {
	t.Helper()
	ctx := context.Background()

	// The transition is the observable finish line: CompleteIntentTransition
	// deletes the row, and it is the last write the demotion makes.
	standing, inFlight, err := f.catalog.GetIntentTransition(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	assert.False(t, inFlight,
		"the demotion left transition %s standing in state %q: %s",
		transitionID, standing.State, standing.LastError)

	checkout, found, err := f.catalog.GetCheckout(ctx, tracked.CheckoutID)
	require.NoError(t, err)
	require.True(t, found, "a demotion keeps the checkout id")
	assert.Equal(t, store_sqlite.CheckoutModeAutomatic, checkout.EffectiveMode)
	assert.Empty(t, checkout.ActiveIntentTransitionID,
		"and releases the transition slot it held")

	route, routed := f.routeOf(tracked.CheckoutID)
	require.True(t, routed)
	assert.Equal(t, f.primaryGraph, route.GraphID, "served from the family primary now")
	assert.Equal(t, store_sqlite.RoutePending, route.State,
		"a route with no layers says so rather than claiming to serve them")

	_, bound, err := f.catalog.GetDedicatedGraph(ctx, GraphIDFor(tracked.Prefix))
	require.NoError(t, err)
	assert.False(t, bound, "the corpus it left is retired")
	assert.Nil(t, f.mi.GetMetadata(tracked.Prefix), "and its rows left the live corpus")
	assert.False(t, f.configLists(f.worktree), "the demoted worktree left the tracked set")
}
