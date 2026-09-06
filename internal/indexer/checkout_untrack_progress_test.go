package indexer

// PURPOSE — a demotion that is working has to be able to say so. Everything
// expensive in an untrack-that-demotes happens behind the published mode flip,
// inside the retirement saga, and the only durable thing an observer can watch
// while it runs is intent_transitions.last_progress. These tests pin the two
// signals that were missing when a demotion of a copied worktree stood at
// running with last_progress one second after created_at for 55 minutes while
// it was in fact grinding through a repository teardown the whole time: the
// transition stamps liveness from inside the saga, and the teardown's long
// pole brackets itself in the log.
// KEYWORDS — untrack, demote, transition, last_progress, liveness, heartbeat,
// contract reconciliation, teardown

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// clockAdvancingNotifier moves a fixture's frozen clock when the lifecycle
// announces that the tracked set changed, and only while its gate agrees.
//
// It is how a test reaches inside the retirement saga without a seam of its
// own: evictRepoChecked invalidates the session scopes once the purge has
// taken the repository's rows out of the live corpus, which is several frames
// below anything a fixture can call directly.
type clockAdvancingNotifier struct {
	clock *manualClock
	step  time.Duration
	gate  func() bool
}

func (n *clockAdvancingNotifier) InvalidateSessionScopes() {
	if n.gate != nil && !n.gate() {
		return
	}
	n.clock.advance(n.step)
}

func (n *clockAdvancingNotifier) RunAnalysis() {}

// TestDemotionStampsLivenessWhileTheRetirementSagaRuns is the regression.
//
// The demotion used to stamp last_progress exactly once, on the way into the
// worker, and then say nothing until the transition row was deleted. Every
// step that can actually take time is on the far side of that stamp — the
// mutation lane drain, the payload purge, the aggregate vector republish, the
// config removal, the contract reconciliation — so a healthy teardown and a
// worker that died holding the transition open are indistinguishable to an
// operator, to the janitor and to a --wait poller, for as long as the teardown
// takes. Measured live: 55 minutes.
//
// The proof is arranged so it cannot pass by accident. The fixture clock is
// frozen, so created_at and every write share one second unless something
// moves it, and the only thing that moves it here is gated on the repository
// having already left the live corpus — which happens inside the purge, inside
// the saga, inside CommitAuthorizedDemotion. A last_progress later than
// created_at is therefore evidence of a stamp written from inside the teardown
// and of nothing else.
func TestDemotionStampsLivenessWhileTheRetirementSagaRuns(t *testing.T) {
	f := newFamilyFixture(t, "demote-liveness")
	defer f.close()
	ctx := context.Background()

	tracked, err := f.lc.Register(ctx, config.RepoEntry{Path: f.worktree}, TrackSourceCLI)
	require.NoError(t, err)
	require.NoError(t, tracked.CatalogErr)

	preview, err := f.lc.PreviewUntrack(ctx, f.worktree)
	require.NoError(t, err)
	require.Equal(t, UntrackPlanDemote, preview.Plan)

	const teardownTick = 20 * time.Minute
	f.lc.SetNotifier(&clockAdvancingNotifier{
		clock: f.clock,
		step:  teardownTick,
		gate:  func() bool { return f.mi.GetMetadata(tracked.Prefix) == nil },
	})

	// The barrier is the one window a fixture can observe mid-demotion: the
	// commit and the saga inside it are through, the finish has not run, and
	// the transition row is still standing.
	var (
		atBarrier  store_sqlite.IntentTransition
		standing   bool
		barrierErr error
	)
	f.lc.demoteBarrier = func() {
		atBarrier, standing, barrierErr = f.catalog.GetIntentTransition(ctx, tracked.CheckoutID)
	}

	result, err := f.lc.StartApplyUntrack(ctx, preview)
	require.NoError(t, err)
	require.True(t, result.Pending)
	f.lc.transitionWG.Wait()

	require.NoError(t, barrierErr)
	require.True(t, standing,
		"the barrier has to run while the transition is still standing, or this test proves nothing")
	require.Equal(t, store_sqlite.IntentTransitionRunning, atBarrier.State)
	require.NotZero(t, atBarrier.CreatedAt)
	assert.Greater(t, atBarrier.LastProgress, atBarrier.CreatedAt,
		"last_progress has to move while the retirement saga works: stamped only on the way in, "+
			"it cannot distinguish a 33-minute purge from a worker that died holding the row")

	assertDemotionSettled(t, f, tracked, result.TransitionID)
}

// TestRepositoryTeardownBracketsItsContractReconciliation pins the other half.
//
// The contract reconciliation is the teardown's long pole — it holds the
// graph's resolve lane across a merged-registry match and an incident-edge
// scan over every tracked repository — and it sits after the config entry has
// already been removed, so an observer watching the config or the repository's
// rows sees a finished untrack while it runs. It used to log nothing at all,
// which is why 33 minutes of a live demotion had no attribution anywhere.
//
// The start line has to carry the frontier's size because that is what
// predicts the cost, and the completion line has to carry the elapsed time
// because that is the number anyone diagnosing this is actually after.
func TestRepositoryTeardownBracketsItsContractReconciliation(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()

	core, logs := observer.New(zap.InfoLevel)
	f.mi.logger = zap.New(core)

	plan := DerivedInvalidationPlan{
		ContractGroups: []ContractGroupFrontier{
			{WorkspaceID: "w", ProjectID: "p", ContractID: "svc.Ping"},
			{WorkspaceID: "w", ProjectID: "p", ContractID: "svc.Pong"},
		},
		ContractSymbolIDs:     []string{"pkg/a.go::Ping"},
		ContractBridgeNodeIDs: []string{"bridge:w:p:svc.Ping"},
	}
	f.mi.reconcileContractEdgesForTeardown("leaving", plan)

	started := logs.FilterMessage("repository teardown: contract reconciliation starting").All()
	require.Len(t, started, 1, "the teardown announces the step before it disappears into it")
	startFields := started[0].ContextMap()
	assert.Equal(t, "leaving", startFields["repo"])
	assert.EqualValues(t, 2, startFields["contract_groups"],
		"the frontier's size is what predicts how long this runs")
	assert.EqualValues(t, 1, startFields["contract_symbol_ids"])
	assert.EqualValues(t, 1, startFields["contract_bridge_node_ids"])

	done := logs.FilterMessage("repository teardown: contract reconciliation complete").All()
	require.Len(t, done, 1, "and says when it came back out")
	doneFields := done[0].ContextMap()
	assert.Equal(t, "leaving", doneFields["repo"])
	assert.Contains(t, doneFields, "elapsed",
		"the elapsed time is the whole point of the second line")
	assert.Contains(t, doneFields, "edges_replaced")
}

// TestRepositoryTeardownSaysNothingWhenThereIsNoContractFrontier keeps the
// bracket from turning every ordinary untrack into two lines of noise. A
// repository that contributed no contracts has nothing to reconcile, and the
// step it is skipping is exactly the one whose cost the log exists to explain.
func TestRepositoryTeardownSaysNothingWhenThereIsNoContractFrontier(t *testing.T) {
	f := newLifecycleFixture(t)
	defer f.close()

	core, logs := observer.New(zap.InfoLevel)
	f.mi.logger = zap.New(core)

	f.mi.reconcileContractEdgesForTeardown("leaving", DerivedInvalidationPlan{})

	assert.Zero(t, logs.FilterMessageSnippet("contract reconciliation").Len(),
		"a skipped step is not news")
}
