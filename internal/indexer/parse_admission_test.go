package indexer

import (
	"context"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestClampParseWeight(t *testing.T) {
	const budget = 1000
	require.Equal(t, int64(1), clampParseWeight(0, budget), "empty file floors to weight 1")
	require.Equal(t, int64(1), clampParseWeight(-5, budget), "negative size floors to weight 1")
	require.Equal(t, int64(500), clampParseWeight(500, budget), "under budget weighs its size")
	require.Equal(t, int64(budget), clampParseWeight(budget, budget), "exactly budget weighs budget")
	require.Equal(t, int64(budget), clampParseWeight(budget*4, budget),
		"a file larger than the whole budget is admitted alone (weight clamped) so it can never deadlock")
}

func TestDefaultParseBudgetEnabled(t *testing.T) {
	require.Positive(t, config.Default().Index.MaxParseBytesInFlight,
		"default config must enable the parse-memory budget so a bare `gortex init` bounds peak memory")
}

func TestParseAdmissionIndexesOversizedFiles(t *testing.T) {
	testParseAdmissionIndexesOversizedFiles(t, false)
}

func TestSharedParseAdmissionIndexesOversizedFiles(t *testing.T) {
	testParseAdmissionIndexesOversizedFiles(t, true)
}

// testParseAdmissionIndexesOversizedFiles verifies neither the private nor the
// shared bytes-in-flight gate drops or deadlocks on a file larger than the
// whole budget: its weight clamps to capacity and it is admitted alone.
func testParseAdmissionIndexesOversizedFiles(t *testing.T, shared bool) {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "big.go"),
		"package p\n\nvar Big = \""+strings.Repeat("x", 8192)+"\"\n")
	for i := 0; i < 8; i++ {
		writeFile(t, filepath.Join(dir, "small"+strconv.Itoa(i)+".go"),
			"package p\n\nfunc F"+strconv.Itoa(i)+"() {}\n")
	}

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	cfg := config.Default().Index
	cfg.MaxParseBytesInFlight = 256
	idx := New(g, reg, cfg, zap.NewNop())
	if shared {
		idx.parseAdmission.Store(newParseAdmissionBudget(256))
	}
	_, err := idx.Index(dir)
	require.NoError(t, err)

	present := map[string]bool{}
	for _, n := range g.AllNodes() {
		if n.Kind == graph.KindFile {
			present[filepath.Base(n.FilePath)] = true
		}
	}
	require.True(t, present["big.go"], "oversized file must still be indexed")
	for i := 0; i < 8; i++ {
		name := "small" + strconv.Itoa(i) + ".go"
		require.True(t, present[name], "small file %s must be indexed", name)
	}
}

func TestSharedParseAdmissionSerializesAcrossPrivateRepoBudgets(t *testing.T) {
	const mib = int64(1 << 20)
	shared := newParseAdmissionBudget(64 * mib)
	localA := semaphore.NewWeighted(64 * mib)
	localB := semaphore.NewWeighted(64 * mib)

	first, err := acquireParseAdmission(t.Context(), 48*mib, 64*mib, localA, shared)
	require.NoError(t, err)

	type outcome struct {
		lease *parseAdmissionLease
		err   error
	}
	secondStarted := make(chan struct{})
	secondDone := make(chan outcome, 1)
	go func() {
		close(secondStarted)
		lease, acquireErr := acquireParseAdmission(t.Context(), 48*mib, 64*mib, localB, shared)
		secondDone <- outcome{lease: lease, err: acquireErr}
	}()
	<-secondStarted
	select {
	case got := <-secondDone:
		got.lease.Release()
		t.Fatalf("second repository passed aggregate budget before release: %v", got.err)
	case <-time.After(50 * time.Millisecond):
	}

	first.Release()
	select {
	case got := <-secondDone:
		require.NoError(t, got.err)
		got.lease.Release()
	case <-time.After(time.Second):
		t.Fatal("second repository did not acquire after shared capacity was released")
	}

	require.True(t, shared.tryAcquire(shared.capacity))
	shared.release(shared.capacity)
}

func TestParseAdmissionCancellationReturnsSharedCapacity(t *testing.T) {
	const mib = int64(1 << 20)
	shared := newParseAdmissionBudget(64 * mib)
	local := semaphore.NewWeighted(1)
	require.NoError(t, local.Acquire(t.Context(), 1))
	defer local.Release(1)

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	lease, err := acquireParseAdmission(ctx, 32*mib, 1, local, shared)
	assert.Nil(t, lease)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, shared.tryAcquire(shared.capacity), "cancelled local admission leaked shared capacity")
	shared.release(shared.capacity)
}

func TestSharedParseAdmissionCancellationWhileWaiting(t *testing.T) {
	shared := newParseAdmissionBudget(1)
	held, err := acquireParseAdmission(t.Context(), 1, 0, nil, shared)
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	waiting, err := acquireParseAdmission(ctx, 1, 0, nil, shared)
	assert.Nil(t, waiting)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	shared.mu.Lock()
	queued := len(shared.waiters)
	shared.mu.Unlock()
	require.Zero(t, queued, "cancelled shared waiter must leave the FIFO queue")

	held.Release()
	require.True(t, shared.tryAcquire(shared.capacity))
	shared.release(shared.capacity)
}

func TestSharedParseAdmissionRejectsPreCancelledImmediateAcquire(t *testing.T) {
	shared := newParseAdmissionBudget(1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	lease, err := acquireParseAdmission(ctx, 1, 0, nil, shared)
	assert.Nil(t, lease)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, shared.tryAcquire(shared.capacity),
		"pre-cancelled acquire must not consume immediately available capacity")
	shared.release(shared.capacity)
}

func TestSharedParseAdmissionCancellationWinsGrantRace(t *testing.T) {
	for range 50 {
		shared := newParseAdmissionBudget(1)
		held, err := acquireParseAdmission(t.Context(), 1, 0, nil, shared)
		require.NoError(t, err)

		ctx, cancel := context.WithCancel(t.Context())
		out := make(chan error, 1)
		go func() {
			lease, acquireErr := acquireParseAdmission(ctx, 1, 0, nil, shared)
			if lease != nil {
				lease.Release()
			}
			out <- acquireErr
		}()
		require.Eventually(t, func() bool {
			shared.mu.Lock()
			defer shared.mu.Unlock()
			return len(shared.waiters) == 1
		}, time.Second, time.Millisecond)

		cancel()
		held.Release()
		require.ErrorIs(t, <-out, context.Canceled)
		require.True(t, shared.tryAcquire(shared.capacity),
			"cancel/grant race must refund the FIFO reservation")
		shared.release(shared.capacity)
	}
}

func TestSharedParseAdmissionLargeWaiterProgressAfterBoundedBypass(t *testing.T) {
	shared := newParseAdmissionBudget(4)
	held, err := acquireParseAdmission(t.Context(), 2, 0, nil, shared)
	require.NoError(t, err)

	largeDone := make(chan *parseAdmissionLease, 1)
	go func() {
		lease, acquireErr := acquireParseAdmission(t.Context(), 4, 0, nil, shared)
		if acquireErr != nil {
			largeDone <- nil
			return
		}
		largeDone <- lease
	}()
	require.Eventually(t, func() bool {
		shared.mu.Lock()
		defer shared.mu.Unlock()
		return len(shared.waiters) == 1
	}, time.Second, time.Millisecond)

	for range maxParseAdmissionBypasses {
		require.True(t, shared.tryAcquire(1), "bounded chunk continuation should use free capacity")
		shared.release(1)
	}
	require.False(t, shared.tryAcquire(1),
		"spent bypass allowance must force active chunks to drain for the FIFO head")

	held.Release()
	select {
	case large := <-largeDone:
		require.NotNil(t, large, "capacity-sized FIFO waiter must make progress")
		large.Release()
	case <-time.After(time.Second):
		t.Fatal("capacity-sized FIFO waiter starved after bounded bypasses drained")
	}
}

func TestSharedParseAdmissionConcurrentInvariant(t *testing.T) {
	const capacity = int64(4)
	shared := newParseAdmissionBudget(capacity)
	var active atomic.Int64
	var violated atomic.Bool
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for iteration := range 200 {
				var lease *parseAdmissionLease
				if (worker+iteration)%3 == 0 {
					for !shared.tryAcquire(1) {
						runtime.Gosched()
					}
					lease = &parseAdmissionLease{shared: shared, sharedWeight: 1}
				} else {
					var err error
					lease, err = acquireParseAdmission(t.Context(), 1, 0, nil, shared)
					if err != nil {
						violated.Store(true)
						return
					}
				}
				if active.Add(1) > capacity {
					violated.Store(true)
				}
				active.Add(-1)
				lease.Release()
			}
		}()
	}
	wg.Wait()
	require.False(t, violated.Load())
	require.Zero(t, active.Load())
	require.True(t, shared.tryAcquire(capacity))
	shared.release(capacity)
}

func TestParseAdmissionReleaseIsIdempotent(t *testing.T) {
	shared := newParseAdmissionBudget(64)
	lease, err := acquireParseAdmission(t.Context(), 32, 0, nil, shared)
	require.NoError(t, err)
	lease.Release()
	lease.Release()
	require.True(t, shared.tryAcquire(shared.capacity))
	shared.release(shared.capacity)
}

func TestPreparedExtractionRetainsSharedAdmissionUntilDiscard(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, "package main\n\nfunc Before() {}\n")

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	idx.storeRootPath(dir)
	shared := newParseAdmissionBudget(1)
	idx.parseAdmission.Store(shared)

	_, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	require.False(t, shared.tryAcquire(shared.capacity),
		"prepared source/tree must retain its shared admission lease")

	idx.discardPreparedExtraction(path)
	require.True(t, shared.tryAcquire(shared.capacity),
		"discard must return the prepared extraction's shared capacity")
	shared.release(shared.capacity)
}

func TestIncrementalChunkFlushesBeforeWaitingForSharedAdmission(t *testing.T) {
	dir := t.TempDir()
	firstPath := filepath.Join(dir, "first.go")
	secondPath := filepath.Join(dir, "second.go")
	writeFile(t, firstPath, "package p\n\nfunc FirstBefore() {}\n")
	writeFile(t, secondPath, "package p\n\nfunc SecondBefore() {}\n")

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	idx := New(g, reg, config.Default().Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	idx.SetDeferGlobalPasses(true)
	shared := newParseAdmissionBudget(1)
	idx.parseAdmission.Store(shared)

	writeFile(t, firstPath, "package p\n\nfunc FirstAfter() {}\n")
	writeFile(t, secondPath, "package p\n\nfunc SecondAfter() {}\n")
	consumed, _, _, failed, _ := idx.reindexIncrementalChunk(
		[]string{firstPath, secondPath}, nil, false,
	)
	require.Equal(t, 1, consumed,
		"a full shared budget must flush the retained partial chunk before the next file")
	require.Empty(t, failed)
	require.True(t, shared.tryAcquire(shared.capacity),
		"committing the partial chunk must release shared capacity")
	shared.release(shared.capacity)

	consumed, _, _, failed, _ = idx.reindexIncrementalChunk([]string{secondPath}, nil, false)
	require.Equal(t, 1, consumed)
	require.Empty(t, failed)

	// Exercise the outer retry loop as well: the busy second file must not be
	// counted as consumed or silently skipped when the first partial chunk
	// flushes under the one-byte shared budget.
	writeFile(t, firstPath, "package p\n\nfunc FirstFinal() {}\n")
	writeFile(t, secondPath, "package p\n\nfunc SecondFinal() {}\n")
	_, reparsed, failed, _ := idx.reindexIncrementalStalePass(
		[]string{firstPath, secondPath}, nil, false,
	)
	require.Empty(t, failed)
	assert.ElementsMatch(t, []string{firstPath, secondPath}, reparsed)

	names := make(map[string]bool)
	for _, node := range g.AllNodes() {
		names[node.Name] = true
	}
	require.True(t, names["FirstFinal"])
	require.True(t, names["SecondFinal"])
}

func TestIncrementalStageReleaseClearsRetainedObjects(t *testing.T) {
	shared := newParseAdmissionBudget(1)
	lease, err := acquireParseAdmission(t.Context(), 1, 0, nil, shared)
	require.NoError(t, err)
	prepared := &preparedExtraction{
		src:        []byte("retained source"),
		result:     &parser.ExtractionResult{},
		parseLease: lease,
	}
	stage := &incrementalBatchStage{
		src:      prepared.src,
		result:   prepared.result,
		prepared: prepared,
	}

	stage.releasePrepared()
	require.Nil(t, stage.src)
	require.Nil(t, stage.result)
	require.Nil(t, stage.prepared)
	require.True(t, shared.tryAcquire(shared.capacity))
	shared.release(shared.capacity)
}

func BenchmarkSharedParseAdmissionUncontended(b *testing.B) {
	shared := newParseAdmissionBudget(512 << 20)
	local := semaphore.NewWeighted(512 << 20)
	b.ReportAllocs()
	for b.Loop() {
		lease, err := acquireParseAdmission(context.Background(), 32<<10, 512<<20, local, shared)
		if err != nil {
			b.Fatal(err)
		}
		lease.Release()
	}
}
