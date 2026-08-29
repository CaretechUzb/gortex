package indexer

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// stormAdoptionWatcher builds a watcher whose storm threshold is crossed part
// way through a burst, which is the production shape: the threshold counts
// EVENTS and a checkout emits several per file, so the per-file path is already
// holding armed timers by the time the window notices the storm.
//
// The debounce is long enough that a timer left armed is not a race, and short
// enough that one still fires well inside the test's own wait. The quiet period
// is effectively infinite so the drain only runs when the test says so.
func stormAdoptionWatcher(t *testing.T, threshold int) (string, *Indexer, *Watcher) {
	t.Helper()
	dir := t.TempDir()
	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(dir)

	w, err := NewWatcher(idx, config.WatchConfig{
		DebounceMs:         200,
		StormThreshold:     threshold,
		StormWindowMs:      60_000,
		StormQuietPeriodMs: 60_000,
	}, zap.NewNop())
	require.NoError(t, err)
	return dir, idx, w
}

// TestWatcherStormEngageAdoptsEveryPendingPointPatch is the direct observation
// that the point-patch path and the batch no longer do the same work twice.
//
// The assertion is the absence of point mutations, so it is made through
// pointMutationClaimed — the hook the debounce callback fires the instant it
// claims its timer — rather than through a proxy like elapsed time or node
// counts. Every path whose timer was armed before the storm engaged must reach
// the batch and must never be claimed by the per-file path.
func TestWatcherStormEngageAdoptsEveryPendingPointPatch(t *testing.T) {
	const threshold = 5
	dir, _, w := stormAdoptionWatcher(t, threshold)

	var claimedMu sync.Mutex
	var claimed []string
	w.pointMutationClaimed = func(path string) {
		claimedMu.Lock()
		claimed = append(claimed, path)
		claimedMu.Unlock()
	}

	const files = 8
	paths := make([]string, 0, files)
	for i := range files {
		path := filepath.Join(dir, fmt.Sprintf("f%02d.go", i))
		writeTestFile(t, path, "package main\n\nfunc Main() {}\n")
		paths = append(paths, path)
	}

	// Events 1..threshold stay on the per-file path and arm a timer each; the
	// next one crosses the window and engages the storm.
	for _, path := range paths {
		w.handleEvent(fakeCreate(path))
	}

	w.mu.Lock()
	pending := len(w.pending)
	w.mu.Unlock()
	require.Zero(t, pending, "engaging the storm must leave no armed per-file timer behind")

	w.stormMu.Lock()
	adopted := w.stormAdopted
	batched := make([]string, 0, len(w.stormBatch))
	for path := range w.stormBatch {
		batched = append(batched, path)
	}
	require.NotNil(t, w.stormTimer)
	w.stopStormTimerLocked()
	w.stormMu.Unlock()

	require.Equal(t, threshold, adopted,
		"every path the per-file path was holding when the storm engaged must be adopted")
	sort.Strings(batched)
	require.Equal(t, paths, batched, "the batch must cover the whole burst")

	// A timer that survived the sweep would fire here. Wait past the debounce
	// before concluding it did not.
	time.Sleep(4 * time.Duration(w.config.DebounceMs) * time.Millisecond)

	claimedMu.Lock()
	defer claimedMu.Unlock()
	require.Empty(t, claimed,
		"an adopted path must never be point-patched: the batch already covers it")
}

// TestWatcherStormEngageCompletesSweptMutationTickets covers the half of the
// sweep that is not visible in the path list. A path adopted because the storm
// engaged — not because its own event arrived — still has callers blocked on
// its MutationTicket, and the batch is now the only thing that can complete
// them: the cancelled callback bails out at its claimPendingTimer identity
// check and completes nothing.
func TestWatcherStormEngageCompletesSweptMutationTickets(t *testing.T) {
	const threshold = 2
	dir, _, w := stormAdoptionWatcher(t, threshold)

	swept := filepath.Join(dir, "swept.go")
	writeTestFile(t, swept, "package main\n\nfunc Swept() {}\n")

	ticket, err := w.EnqueueFileMutation(context.Background(), swept)
	require.NoError(t, err)
	require.NotNil(t, ticket)

	// Drive the window over the threshold with other paths, so `swept` is only
	// ever reached by the sweep.
	var burst []string
	for i := range threshold + 1 {
		path := filepath.Join(dir, fmt.Sprintf("b%02d.go", i))
		writeTestFile(t, path, "package main\n\nfunc Burst() {}\n")
		burst = append(burst, path)
		w.handleEvent(fakeCreate(path))
	}

	var gotPaths []string
	w.batchReindex = func(paths []string) (*IndexResult, error) {
		gotPaths = append([]string(nil), paths...)
		return &IndexResult{StaleFileCount: len(paths)}, nil
	}

	w.stormMu.Lock()
	// `swept`, plus the burst paths whose own timers were armed before the
	// event that engaged the storm.
	require.Equal(t, threshold+1, w.stormAdopted)
	require.NotNil(t, w.stormTimer)
	w.stopStormTimerLocked()
	w.stormMu.Unlock()

	w.drainStorm()

	require.Contains(t, gotPaths, swept, "a swept path must be indexed by the batch")
	require.Subset(t, gotPaths, burst)

	select {
	case result, ok := <-ticket.Done:
		require.True(t, ok)
		require.Equal(t, ticket.Generation, result.RequestedGeneration)
		require.True(t, result.Reindexed)
		require.NoError(t, result.Err)
	default:
		t.Fatal("swept ticket did not complete through the storm batch")
	}
}
