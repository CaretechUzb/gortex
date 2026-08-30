package indexer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The window between "a derivation is owed" and "the passes opened" is not a
// tick: the scheduler debounces, republishes the checkout grouping, runs a
// whole-workspace cross-repo resolve and waits on the batch-mutation gate
// before anything calls DeriveBegan. A repo tracked into that window has no
// derive_state row yet, so with nothing published it reads "never derived" —
// the verdict reserved for a graph that is silently wrong — while the daemon
// is in fact working on exactly that repo.

// The first schedule publishes before it starts anything. The assertion is
// order-based, not timing-based: scheduleWorkspaceRederive publishes under the
// scheduler lock and launches the goroutine only after releasing it, so the
// first published set is always the one this test names.
func TestARepoIsOwedFromTheMomentItIsScheduled(t *testing.T) {
	marker := &fakeMarker{}
	mi := deriveTestIndexer(newDeriveStateGraph(), "repoA")
	mi.SetRuntimeMarker(marker)
	// Long enough that the pass cannot open behind the assertions, short
	// enough that teardown does not wait on it — time.Sleep in the scheduler
	// loop is not cancellable, so this is the whole cost of stopping.
	mi.rederive.debounce = 500 * time.Millisecond

	mi.scheduleWorkspaceRederive("repoA")

	published := marker.publishedOwedSets()
	require.NotEmpty(t, published, "scheduling must publish the repo it owes")
	require.Equal(t, []string{"repoA"}, published[0])
	require.Empty(t, marker.opened,
		"the pass must not have opened — the un-opened window is what is under test")

	mi.stopWorkspaceRederive()
	require.Empty(t, marker.owedNow(),
		"a closed scheduler owes nothing; leaving the set published would strand a "+
			"reader on a promise no pass will keep")
}

// A track landing while a pass runs is queued behind it, and the running pass
// cannot cover it — it may already have read past that repository's nodes. So
// it is owed for the remainder of the current pass plus all of the next one.
func TestARepoQueuedBehindARunningPassIsOwed(t *testing.T) {
	marker := &fakeMarker{}
	mi := deriveTestIndexer(newDeriveStateGraph(), "repoA")
	mi.SetRuntimeMarker(marker)
	// Stand in for a pass in flight. No goroutine starts, so the scheduler
	// takes the queue branch and nothing races the assertion.
	mi.rederive.running = true

	mi.scheduleWorkspaceRederive("repoB")

	require.Equal(t, []string{"repoB"}, marker.owedNow())
	require.Empty(t, marker.opened)
}

// A repo tracked inside a batch is deferred, not scheduled: the batch owns the
// gates a derivation needs. It is owed for the whole life of that batch, which
// on a daemon warm start is the entire warmup — the window that once left a
// repo underived permanently and unreported.
func TestARepoDeferredBehindABatchIsOwed(t *testing.T) {
	marker := &fakeMarker{}
	mi := deriveTestIndexer(newDeriveStateGraph(), "repoA")
	mi.SetRuntimeMarker(marker)

	mi.deferWorkspaceRederive("repoA")
	require.Equal(t, []string{"repoA"}, marker.owedNow())

	// EndBatch's own global pass IS the derivation these were waiting for, so
	// clearing the set must retire the marker with it.
	mi.ClearDeferredWorkspaceRederive()
	require.Empty(t, marker.owedNow())
}

// The frontier leaves pending the moment a pass takes it, minutes before that
// pass opens. Publishing pending alone would blink the repo out of the owed
// set for exactly the interval the marker exists to cover, so the union spans
// the hand-off.
func TestTheOwedSetSpansPendingInflightAndDeferred(t *testing.T) {
	s := &workspaceRederiveScheduler{
		pending:  map[string]struct{}{"repoB": {}},
		inflight: map[string]struct{}{"repoA": {}},
		deferred: map[string]struct{}{"repoC": {}, "repoA": {}},
	}
	require.Equal(t, []string{"repoA", "repoB", "repoC"}, s.owedLocked(),
		"sorted and deduped: a repo both in flight and deferred is owed once")
	require.Empty(t, (&workspaceRederiveScheduler{}).owedLocked())
}
