package indexer

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/trigram"
	"go.uber.org/zap"
)

// The trigram searcher is a full-text in-memory index built per repo and
// previously never released. These pin the two ceilings: idle entries are
// dropped, and no more than maxLive repos hold one at once.

func newTestTrigramBudget(ttl time.Duration, maxLive int, clock *time.Time) *trigramBudget {
	b := newTrigramBudget(ttl, maxLive, -1)
	b.now = func() time.Time { return *clock }
	// These tests exercise eviction, not promotion: build on first demand
	// so a searcher exists to evict. Promotion has its own test.
	b.promoteAfter = 1
	// Fake-clock tests drive expiry by touch; do not arm a real timer against
	// an artificial wall clock.
	b.afterFunc = nil
	return b
}

func TestTrigramBudgetEvictsLoneIdleOwnerWithoutAnotherTouch(t *testing.T) {
	b := newTrigramBudget(20*time.Millisecond, 10, -1)
	released := make(chan struct{})
	b.touch(&Indexer{}, func() { close(released) }, 0)

	select {
	case <-released:
	case <-time.After(2 * time.Second):
		t.Fatal("idle trigram cache was not released at its own deadline")
	}
	require.Zero(t, b.live())
}

func TestTrigramBudgetReusesOneDeadlineTimer(t *testing.T) {
	b := newTrigramBudget(time.Hour, 10, -1)
	owner := &Indexer{}
	b.touch(owner, func() {}, 0)
	first := b.timer
	require.NotNil(t, first)

	b.touch(owner, func() {}, 0)
	require.Same(t, first, b.timer, "touches must reset one timer, not allocate one per query")
	b.forget(owner)
}

func TestTrigramBudgetEvictsIdleOwners(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newTestTrigramBudget(time.Minute, 10, &now)

	idle, active := &Indexer{}, &Indexer{}
	var released []string
	b.touch(idle, func() { released = append(released, "idle") }, 0)
	b.touch(active, func() { released = append(released, "active") }, 0)
	require.Equal(t, 2, b.live())

	// Only `active` keeps being used; `idle` ages past the TTL.
	now = now.Add(90 * time.Second)
	b.touch(active, func() { released = append(released, "active") }, 0)

	require.Equal(t, []string{"idle"}, released, "the idle repo's index must be dropped")
	require.Equal(t, 1, b.live())
}

func TestTrigramBudgetCapsLiveOwners(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// A generous TTL so only the count ceiling can be doing the work here.
	b := newTestTrigramBudget(time.Hour, 2, &now)

	first, second, third := &Indexer{}, &Indexer{}, &Indexer{}
	var released []string
	b.touch(first, func() { released = append(released, "first") }, 0)
	now = now.Add(time.Second)
	b.touch(second, func() { released = append(released, "second") }, 0)
	now = now.Add(time.Second)
	b.touch(third, func() { released = append(released, "third") }, 0)

	require.Equal(t, []string{"first"}, released,
		"over the cap, the least recently used repo is evicted")
	require.Equal(t, 2, b.live())
}

func TestTrigramBudgetNeverEvictsTheCallerBeingWarmed(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newTestTrigramBudget(time.Nanosecond, 1, &now)

	warming := &Indexer{}
	var releasedSelf bool
	b.touch(warming, func() { releasedSelf = true }, 0)
	now = now.Add(time.Hour)
	// Even with everything aged out and a cap of one, the repo whose search
	// is being served right now must keep the index it just paid to build.
	b.touch(warming, func() { releasedSelf = true }, 0)

	require.False(t, releasedSelf, "the caller's own index must survive its own touch")
	require.Equal(t, 1, b.live())
}

func TestTrigramBudgetForgetDoesNotCallBack(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newTestTrigramBudget(time.Minute, 10, &now)

	owner, other := &Indexer{}, &Indexer{}
	var released bool
	b.touch(owner, func() { released = true }, 0)
	b.forget(owner)
	require.Zero(t, b.live())

	// Ageing everything out must not reach the forgotten owner.
	now = now.Add(time.Hour)
	b.touch(other, func() {}, 0)
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
	}, 0)
	now = now.Add(time.Hour)

	done := make(chan struct{})
	var once sync.Once
	go func() {
		b.touch(fresh, func() {}, 0)
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

func TestStaleTrigramLeaseCannotReleaseRetouchedSearcher(t *testing.T) {
	idx := &Indexer{trigramSearcher: &trigram.Searcher{}, trigramGen: 7}
	idx.trigramMu.Lock()
	stale := idx.trigramReleaseLocked()
	current := idx.trigramReleaseLocked()
	idx.trigramMu.Unlock()

	stale()
	require.NotNil(t, idx.trigramSearcher)
	require.Equal(t, uint64(7), idx.trigramGen)

	current()
	require.Nil(t, idx.trigramSearcher)
	require.Zero(t, idx.trigramGen)
}

func TestBoundedWarmSearchRefreshesTrigramBudget(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	budget := newTestTrigramBudget(time.Minute, 1, &now)
	dir := t.TempDir()
	writeTestFile(t, dir+"/main.go", "package main\n\nfunc Warm() int { return 0 }\n")
	idx := &Indexer{
		rootPath:              dir,
		fileMtimes:            map[string]int64{"main.go": 1},
		trigramBudgetOverride: budget,
	}
	require.NotNil(t, idx.warmTrigramSearcher())

	now = now.Add(30 * time.Second)
	idx.GrepTextBounded(context.Background(), "Warm", 10, 10)

	budget.mu.Lock()
	lastUsed := budget.entries[idx].lastUsed
	budget.mu.Unlock()
	require.Equal(t, now, lastUsed, "bounded-only activity must keep a warm cache alive")

	now = now.Add(10 * time.Second)
	idx.GrepLiteralBounded(context.Background(), "Warm", 10, 10)
	budget.mu.Lock()
	lastUsed = budget.entries[idx].lastUsed
	budget.mu.Unlock()
	require.Equal(t, now, lastUsed, "bounded literal activity must also refresh the cache")
}

func TestUntrackRepoForgetsAndReleasesTrigramCache(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	budget := newTestTrigramBudget(time.Hour, 1, &now)
	idx := &Indexer{
		trigramSearcher:       &trigram.Searcher{},
		trigramGen:            7,
		trigramBudgetOverride: budget,
	}
	idx.trigramMu.Lock()
	release := idx.trigramReleaseLocked()
	idx.trigramMu.Unlock()
	budget.touch(idx, release, 0)

	mi := &MultiIndexer{
		graph:    graph.New(),
		repos:    map[string]*RepoMetadata{"repo": {}},
		indexers: map[string]*Indexer{"repo": idx},
		logger:   zap.NewNop(),
	}
	mi.UntrackRepo("repo")

	require.Zero(t, budget.live())
	require.Nil(t, idx.trigramSearcher)
	require.Zero(t, idx.trigramGen)
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

// TestTrigramBudgetEvictsOnBytes covers the ceiling a count cap cannot
// provide: three indexes of an arbitrarily large repo is still arbitrarily
// large, so the summed footprint needs its own rule.
func TestTrigramBudgetEvictsOnBytes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newTestTrigramBudget(time.Hour, 10, &now)
	b.maxBytes = 100

	var released []string
	small, big := &Indexer{}, &Indexer{}

	b.touch(small, func() { released = append(released, "small") }, 40)
	now = now.Add(time.Second)
	b.touch(big, func() { released = append(released, "big") }, 90)

	// 40 + 90 is over the 100-byte ceiling and small is the LRU entry.
	if len(released) != 1 || released[0] != "small" {
		t.Fatalf("released = %v, want [small]", released)
	}
	if b.live() != 1 {
		t.Errorf("live = %d, want 1", b.live())
	}
	if got := b.stats().Bytes; got != 90 {
		t.Errorf("stats().Bytes = %d, want 90 — the total must track evictions", got)
	}
	if got := b.stats().Evictions; got != 1 {
		t.Errorf("stats().Evictions = %d, want 1", got)
	}
}

// TestTrigramBudgetNeverEvictsTheCaller pins the deliberate choice that a
// repo larger than the whole budget parks over it rather than thrashing:
// evicting the build being paid for right now would rebuild it immediately.
func TestTrigramBudgetNeverEvictsTheCaller(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newTestTrigramBudget(time.Hour, 10, &now)
	b.maxBytes = 10

	released := false
	huge := &Indexer{}
	b.touch(huge, func() { released = true }, 1<<30)

	if released {
		t.Error("the budget evicted the searcher its own caller is using")
	}
	if b.live() != 1 {
		t.Errorf("live = %d, want 1", b.live())
	}
}

// TestTrigramBudgetReTouchAdjustsBytes verifies the byte total follows a
// repo whose index grew or shrank between searches rather than double
// counting it.
func TestTrigramBudgetReTouchAdjustsBytes(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	b := newTestTrigramBudget(time.Hour, 10, &now)

	owner := &Indexer{}
	b.touch(owner, func() {}, 100)
	b.touch(owner, func() {}, 250)
	if got := b.stats().Bytes; got != 250 {
		t.Errorf("Bytes after re-touch = %d, want 250", got)
	}

	b.forget(owner)
	if got := b.stats().Bytes; got != 0 {
		t.Errorf("Bytes after forget = %d, want 0", got)
	}
}

// TestTrigramBudgetBuildsOff covers GORTEX_TRIGRAM_MAX_LIVE=0: the operator
// asked for no in-memory index at all, so nothing may build one.
func TestTrigramBudgetBuildsOff(t *testing.T) {
	b := newTrigramBudget(time.Hour, 0, -1)
	if b.allowsBuild() {
		t.Fatal("allowsBuild = true with maxLive 0, want false")
	}
	if s := b.stats(); !s.BuildsOff || s.MaxLive != 0 {
		t.Errorf("stats = %+v, want BuildsOff with MaxLive 0", s)
	}

	idx := &Indexer{trigramBudgetOverride: b}
	if got := idx.warmTrigramSearcher(); got != nil {
		t.Error("warmTrigramSearcher built an index while builds are off")
	}

	// The default budget still builds.
	if !newTrigramBudget(time.Hour, -1, -1).allowsBuild() {
		t.Error("default budget refuses to build")
	}
}

func TestTrigramBudgetEnvParsing(t *testing.T) {
	t.Setenv("GORTEX_TRIGRAM_MAX_LIVE", "0")
	if got := trigramMaxLiveFromEnv(); got != 0 {
		t.Errorf("max live = %d, want 0 honoured as never-build", got)
	}
	t.Setenv("GORTEX_TRIGRAM_MAX_LIVE", "7")
	if got := trigramMaxLiveFromEnv(); got != 7 {
		t.Errorf("max live = %d, want 7", got)
	}
	t.Setenv("GORTEX_TRIGRAM_MAX_LIVE", "-3")
	if got := trigramMaxLiveFromEnv(); got != defaultTrigramMaxLive {
		t.Errorf("max live = %d, want the default for a negative value", got)
	}

	t.Setenv("GORTEX_TRIGRAM_MAX_MB", "128")
	if got := trigramMaxBytesFromEnv(); got != 128<<20 {
		t.Errorf("max bytes = %d, want 128 MiB", got)
	}
	t.Setenv("GORTEX_TRIGRAM_MAX_MB", "0")
	if got := trigramMaxBytesFromEnv(); got != 0 {
		t.Errorf("max bytes = %d, want 0 honoured as no byte ceiling", got)
	}
	t.Setenv("GORTEX_TRIGRAM_MAX_MB", "not-a-number")
	if got := trigramMaxBytesFromEnv(); got != defaultTrigramMaxBytes {
		t.Errorf("max bytes = %d, want the default for junk", got)
	}
}

// TestWarmTrigramSearcherRequiresDemand covers the promotion gate: a repo
// brushed once by an unscoped fan-out must not build an index, or one
// search across N repos builds N corpora only for the budget to evict
// almost all of them. Searches before promotion still answer, by
// streaming.
func TestWarmTrigramSearcherRequiresDemand(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	budget := newTestTrigramBudget(time.Hour, 3, &now)
	budget.promoteAfter = 3
	budget.promoteWindow = time.Minute

	dir := t.TempDir()
	writeTestFile(t, dir+"/main.go", "package main\n\nfunc Warm() int { return 0 }\n")
	idx := &Indexer{
		rootPath:              dir,
		fileMtimes:            map[string]int64{"main.go": 1},
		trigramBudgetOverride: budget,
	}

	require.Nil(t, idx.warmTrigramSearcher(), "first search must not build")
	require.Nil(t, idx.warmTrigramSearcher(), "second search must not build")
	require.Zero(t, budget.live(), "an unpromoted repo holds no index")

	// The unpromoted searches still return results, via the streaming path.
	matches := idx.GrepText("Warm", 10)
	require.NotEmpty(t, matches, "streaming must answer while unpromoted")

	require.NotNil(t, idx.warmTrigramSearcher(), "sustained demand earns an index")
	require.Equal(t, 1, budget.live())

	// Once built, further searches reuse it without re-earning.
	require.NotNil(t, idx.warmTrigramSearcher())
	require.Equal(t, 1, budget.live())
}

// TestTrigramDemandDecays verifies demand is windowed: a repo touched once
// every so often never accumulates enough to be promoted.
func TestTrigramDemandDecays(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	budget := newTestTrigramBudget(time.Hour, 3, &now)
	budget.promoteAfter = 3
	budget.promoteWindow = time.Minute

	idx := &Indexer{}
	for i := 0; i < 5; i++ {
		require.False(t, budget.earnsBuild(idx), "isolated searches must not promote")
		now = now.Add(2 * time.Minute)
	}
}

// TestWarmTrigramSearcherPatchesDirtyFiles covers the incremental path: an
// index-generation bump with a small changed set patches those files
// instead of rebuilding the whole corpus.
func TestWarmTrigramSearcherPatchesDirtyFiles(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	budget := newTestTrigramBudget(time.Hour, 3, &now)

	dir := t.TempDir()
	writeTestFile(t, dir+"/main.go", "package main\n\nfunc Original() int { return 0 }\n")
	idx := &Indexer{
		rootPath:              dir,
		fileMtimes:            map[string]int64{"main.go": 1},
		trigramBudgetOverride: budget,
	}
	require.NotNil(t, idx.warmTrigramSearcher())
	built := idx.trigramSearcher

	writeTestFile(t, dir+"/main.go", "package main\n\nfunc Renamed() int { return 0 }\n")
	idx.noteTrigramDirty("main.go")
	idx.indexGen.Add(1)

	require.NotNil(t, idx.warmTrigramSearcher())
	require.Same(t, built, idx.trigramSearcher, "a small delta must patch, not rebuild")
	require.Empty(t, idx.trigramDirty, "the dirty set is consumed by the patch")
	require.NotEmpty(t, idx.GrepText("Renamed", 10), "the edit is visible")
	require.Empty(t, idx.GrepText("Original", 10), "the old content is gone")
}

// TestWarmTrigramSearcherRebuildsOnLargeDelta pins the other side of the
// patch decision: past the patch limit a single rebuild is cheaper.
func TestWarmTrigramSearcherRebuildsOnLargeDelta(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	budget := newTestTrigramBudget(time.Hour, 3, &now)

	dir := t.TempDir()
	writeTestFile(t, dir+"/main.go", "package main\n")
	idx := &Indexer{
		rootPath:              dir,
		fileMtimes:            map[string]int64{"main.go": 1},
		trigramBudgetOverride: budget,
	}
	require.NotNil(t, idx.warmTrigramSearcher())
	built := idx.trigramSearcher

	dirty := make([]string, trigramPatchLimit+1)
	for i := range dirty {
		dirty[i] = "generated/f" + strconv.Itoa(i) + ".go"
	}
	idx.noteTrigramDirty(dirty...)
	idx.indexGen.Add(1)

	require.NotNil(t, idx.warmTrigramSearcher())
	require.NotSame(t, built, idx.trigramSearcher, "a large delta must rebuild")
}
