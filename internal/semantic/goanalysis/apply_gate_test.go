package goanalysis

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestFullRepoApplyGateCancellationDoesNotConsumeSlot(t *testing.T) {
	provider := newTestProvider(t)
	firstRelease, err := provider.acquireFullRepoApply(context.Background())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = provider.acquireFullRepoApply(ctx)
	require.ErrorIs(t, err, context.Canceled)

	firstRelease()
	finalRelease, err := provider.acquireFullRepoApply(context.Background())
	require.NoError(t, err, "a cancelled waiter must not consume the apply slot")
	finalRelease()
}

func TestCompilerAdmissionReleasesBeforeFullApplyWait(t *testing.T) {
	provider := newTestProvider(t)
	blockApply, err := provider.acquireFullRepoApply(context.Background())
	require.NoError(t, err)
	defer blockApply()

	releaseCompilerAdmission, err := provider.acquireHeavy(context.Background(), true)
	require.NoError(t, err)
	compilerReleased := make(chan struct{})
	transitionDone := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		releaseApply, _, transitionErr := provider.transitionToFullRepoApply(ctx, func() {
			releaseCompilerAdmission()
			close(compilerReleased)
		})
		if releaseApply != nil {
			releaseApply()
		}
		transitionDone <- transitionErr
	}()

	select {
	case <-compilerReleased:
	case <-time.After(time.Second):
		t.Fatal("compiler admission remained held while the apply gate was queued")
	}

	admissionCtx, admissionCancel := context.WithTimeout(context.Background(), time.Second)
	defer admissionCancel()
	nextCompilerRelease, err := provider.acquireHeavy(admissionCtx, true)
	require.NoError(t, err, "the next compiler must be admitted while detached apply is queued")
	nextCompilerRelease()

	cancel()
	require.ErrorIs(t, <-transitionDone, context.Canceled)
}

func TestFullRepoApplyGatePreventsApplyOverlap(t *testing.T) {
	provider := newTestProvider(t)
	var active atomic.Int32
	var maximum atomic.Int32
	firstEntered := make(chan struct{})
	secondAttempting := make(chan struct{})
	secondEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(2)

	go func() {
		defer workers.Done()
		release, err := provider.acquireFullRepoApply(context.Background())
		require.NoError(t, err)
		defer release()
		now := active.Add(1)
		maximum.CompareAndSwap(0, now)
		close(firstEntered)
		<-releaseFirst
		active.Add(-1)
	}()

	<-firstEntered
	go func() {
		defer workers.Done()
		close(secondAttempting)
		release, err := provider.acquireFullRepoApply(context.Background())
		require.NoError(t, err)
		defer release()
		now := active.Add(1)
		for old := maximum.Load(); now > old && !maximum.CompareAndSwap(old, now); old = maximum.Load() {
		}
		close(secondEntered)
		active.Add(-1)
	}()

	<-secondAttempting
	select {
	case <-secondEntered:
		t.Fatal("two full-repository apply sections overlapped")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirst)
	workers.Wait()
	require.Equal(t, int32(1), maximum.Load())
}

func TestFullRepoApplyWaitDoesNotHoldResolveMutex(t *testing.T) {
	provider := newTestProvider(t)
	blockApply, err := provider.acquireFullRepoApply(context.Background())
	require.NoError(t, err)
	defer blockApply()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waiterStarted := make(chan struct{})
	waiterDone := make(chan error, 1)
	go func() {
		close(waiterStarted)
		release, waitErr := provider.acquireFullRepoApply(ctx)
		if release != nil {
			release()
		}
		waiterDone <- waitErr
	}()
	<-waiterStarted

	resolveMu := graph.New().ResolveMutex()
	locked := make(chan struct{})
	go func() {
		resolveMu.Lock()
		close(locked)
		resolveMu.Unlock()
	}()
	select {
	case <-locked:
	case <-time.After(time.Second):
		t.Fatal("waiting for full apply blocked the graph resolve mutex")
	}

	cancel()
	require.ErrorIs(t, <-waiterDone, context.Canceled)
}

func TestIncrementalEnrichmentDoesNotWaitForFullRepoApply(t *testing.T) {
	provider := newTestProvider(t)
	blockApply, err := provider.acquireFullRepoApply(context.Background())
	require.NoError(t, err)
	defer blockApply()

	root := resolvedTempDir(t)
	writeGoMod(t, root, "example.com/incremental")
	writeFile(t, root, "main.go", "package incremental\n\nfunc Changed() {}\n")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = provider.EnrichFilesContext(ctx, graph.New(), "", root, []string{"main.go"})
	require.NoError(t, err)
}

// BenchmarkFullRepoApplyContention models two repositories repeatedly handing
// ResolveMutex/SQLite ownership back and forth. The yield burst represents the
// scheduler and hot-state disruption at an ownership change; steady work inside
// one repository has no artificial delay. This is a coordination benchmark, not
// a substitute for the end-to-end cold-index gate.
func BenchmarkFullRepoApplyContention(b *testing.B) {
	for _, coordinated := range []bool{false, true} {
		name := "uncoordinated"
		if coordinated {
			name = "coordinated"
		}
		b.Run(name, func(b *testing.B) {
			provider := NewProvider(ModeTypeCheck, false, nil)
			b.ReportAllocs()
			var totalWait atomic.Int64
			var totalHandoffs atomic.Int64
			b.ResetTimer()
			for iteration := 0; iteration < b.N; iteration++ {
				var resolveMu sync.Mutex
				lastOwner := -1
				start := make(chan struct{})
				var workers sync.WaitGroup
				workers.Add(2)
				for owner := 0; owner < 2; owner++ {
					owner := owner
					go func() {
						defer workers.Done()
						<-start
						var release func()
						if coordinated {
							var err error
							release, err = provider.acquireFullRepoApply(context.Background())
							if err != nil {
								b.Error(err)
								return
							}
							defer release()
						}
						for slice := 0; slice < 128; slice++ {
							waitStart := time.Now()
							resolveMu.Lock()
							totalWait.Add(int64(time.Since(waitStart)))
							if lastOwner != owner {
								totalHandoffs.Add(1)
								lastOwner = owner
								for spin := 0; spin < 32; spin++ {
									runtime.Gosched()
								}
							}
							resolveMu.Unlock()
							runtime.Gosched()
						}
					}()
				}
				close(start)
				workers.Wait()
			}
			b.StopTimer()
			b.ReportMetric(float64(totalWait.Load())/float64(b.N), "resolve-wait-ns/op")
			b.ReportMetric(float64(totalHandoffs.Load())/float64(b.N), "handoffs/op")
		})
	}
}
