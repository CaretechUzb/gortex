package indexer

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The trigram searcher is a full-text in-memory index built per repo and
// previously never released. These pin the two ceilings: idle entries are
// dropped, and no more than maxLive repos hold one at once.

func newTestTrigramBudget(ttl time.Duration, maxLive int, clock *time.Time) *trigramBudget {
	b := newTrigramBudget(ttl, maxLive)
	b.now = func() time.Time { return *clock }
	return b
}

func TestTrigramBudgetEvictsIdleOwners(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newTestTrigramBudget(time.Minute, 10, &now)

	idle, active := &Indexer{}, &Indexer{}
	var released []string
	b.touch(idle, func() { released = append(released, "idle") })
	b.touch(active, func() { released = append(released, "active") })
	require.Equal(t, 2, b.live())

	// Only `active` keeps being used; `idle` ages past the TTL.
	now = now.Add(90 * time.Second)
	b.touch(active, func() { released = append(released, "active") })

	require.Equal(t, []string{"idle"}, released, "the idle repo's index must be dropped")
	require.Equal(t, 1, b.live())
}

func TestTrigramBudgetCapsLiveOwners(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// A generous TTL so only the count ceiling can be doing the work here.
	b := newTestTrigramBudget(time.Hour, 2, &now)

	first, second, third := &Indexer{}, &Indexer{}, &Indexer{}
	var released []string
	b.touch(first, func() { released = append(released, "first") })
	now = now.Add(time.Second)
	b.touch(second, func() { released = append(released, "second") })
	now = now.Add(time.Second)
	b.touch(third, func() { released = append(released, "third") })

	require.Equal(t, []string{"first"}, released,
		"over the cap, the least recently used repo is evicted")
	require.Equal(t, 2, b.live())
}

func TestTrigramBudgetNeverEvictsTheCallerBeingWarmed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newTestTrigramBudget(time.Nanosecond, 1, &now)

	warming := &Indexer{}
	var releasedSelf bool
	b.touch(warming, func() { releasedSelf = true })
	now = now.Add(time.Hour)
	// Even with everything aged out and a cap of one, the repo whose search
	// is being served right now must keep the index it just paid to build.
	b.touch(warming, func() { releasedSelf = true })

	require.False(t, releasedSelf, "the caller's own index must survive its own touch")
	require.Equal(t, 1, b.live())
}

func TestTrigramBudgetForgetDoesNotCallBack(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newTestTrigramBudget(time.Minute, 10, &now)

	owner, other := &Indexer{}, &Indexer{}
	var released bool
	b.touch(owner, func() { released = true })
	b.forget(owner)
	require.Zero(t, b.live())

	// Ageing everything out must not reach the forgotten owner.
	now = now.Add(time.Hour)
	b.touch(other, func() {})
	require.False(t, released, "a forgotten owner must never have its release invoked")
}

// Release callbacks take the owning Indexer's trigramMu, so the budget must
// not hold its own lock while running them. Under -race a re-entrant call
// would deadlock rather than fail an assertion.
func TestTrigramBudgetRunsReleaseOutsideItsLock(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newTestTrigramBudget(time.Nanosecond, 1, &now)

	stale, fresh := &Indexer{}, &Indexer{}
	b.touch(stale, func() {
		// Re-entering the budget from a release callback must not deadlock.
		b.live()
	})
	now = now.Add(time.Hour)

	done := make(chan struct{})
	var once sync.Once
	go func() {
		b.touch(fresh, func() {})
		once.Do(func() { close(done) })
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("touch deadlocked: release ran while the budget lock was held")
	}
}

func TestReleaseTrigramSearcherIsSafeWhenNothingBuilt(t *testing.T) {
	idx := &Indexer{}
	require.NotPanics(t, idx.releaseTrigramSearcher)
	require.Nil(t, idx.trigramSearcher)
	require.Zero(t, idx.trigramGen, "a released searcher must not keep a generation that would look warm")
}

// End-to-end over the real warm path: building a searcher in one repo must
// evict another repo's, so the number of live full-text indexes stays capped
// no matter how many repos get searched.
func TestWarmTrigramSearcherEvictsOtherReposOverCap(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	budget := newTestTrigramBudget(time.Hour, 1, &now)

	newRepo := func(name string) *Indexer {
		dir := t.TempDir()
		writeTestFile(t, dir+"/main.go", "package main\n\nfunc "+name+"() int { return 0 }\n")
		return &Indexer{
			rootPath:              dir,
			fileMtimes:            map[string]int64{"main.go": 1},
			trigramBudgetOverride: budget,
		}
	}

	first, second := newRepo("First"), newRepo("Second")

	require.NotNil(t, first.warmTrigramSearcher())
	require.NotNil(t, first.trigramSearcher, "the repo just searched holds its index")

	now = now.Add(time.Second)
	require.NotNil(t, second.warmTrigramSearcher())

	require.NotNil(t, second.trigramSearcher, "the repo just searched holds its index")
	require.Nil(t, first.trigramSearcher,
		"over the cap, an earlier repo's full-text index must be released")
	require.Equal(t, 1, budget.live())

	// Evicted is not broken: the next search in that repo rebuilds it.
	now = now.Add(time.Second)
	require.NotNil(t, first.warmTrigramSearcher())
	require.NotNil(t, first.trigramSearcher, "an evicted repo rebuilds on its next search")
}
