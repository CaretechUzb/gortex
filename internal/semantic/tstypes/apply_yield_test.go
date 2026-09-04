package tstypes

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/semantic"
)

// multiPageSpool stages enough files that the apply's phase loop runs more than
// one page, which is the only place a yield can happen.
func multiPageSpool(t *testing.T, files int) *factSpool {
	t.Helper()
	spool, err := newFactSpool()
	require.NoError(t, err)
	t.Cleanup(spool.close)

	batch := make([]stagedFileFacts, 0, files)
	for i := 0; i < files; i++ {
		record, encodeErr := stageFileFacts(&fileFacts{
			file:       fmt.Sprintf("repo/src/f%05d.py", i),
			repoPrefix: "repo",
			supers: []superFact{{
				typeName: fmt.Sprintf("T%d", i), superName: "Base",
				kind: graph.EdgeExtends, line: 1,
			}},
			metas: []metaFact{{key: "return_type", value: "Result", name: "run", line: 2}},
		})
		require.NoError(t, encodeErr)
		batch = append(batch, record)
	}
	require.NoError(t, spool.appendFiles(batch))
	return spool
}

// applyWithContender runs applyStagedFacts while a second goroutine waits on the
// store-wide resolve mutex, and reports whether that goroutine got in while the
// apply was still running.
//
// yielding false reproduces the pre-fix apply, which held the mutex from its
// first page to its last. queued says whether the contender publishes its wait
// in the process-wide resolve queue the way lockResolveQueued does — the
// difference between a resolver pass (which the apply must let in) and a
// sibling enrichment apply (which it must not).
func applyWithContender(t *testing.T, yielding, queued bool) bool {
	t.Helper()
	g := graph.New()
	// Give the apply a real repo to walk so its phases have work to do.
	for i := 0; i < 3*tstypesFactPageFiles; i++ {
		g.AddNode(&graph.Node{
			ID: fmt.Sprintf("repo/src/f%05d.py::T%d", i, i), Kind: graph.KindType,
			Name: fmt.Sprintf("T%d", i), FilePath: fmt.Sprintf("repo/src/f%05d.py", i),
			Language: "python", RepoPrefix: "repo",
		})
	}

	p := NewProvider(PythonSpec(), zap.NewNop())
	spool := multiPageSpool(t, 3*tstypesFactPageFiles)

	mu := g.ResolveMutex()
	var gotIn atomic.Bool
	var contender sync.WaitGroup
	contender.Add(1)
	mu.Lock()
	go func() {
		defer contender.Done()
		var id uint64
		if queued {
			id = graph.EnterResolveQueue("test resolver")
		}
		mu.Lock()
		if queued {
			graph.LeaveResolveQueue(id)
		}
		gotIn.Store(true)
		mu.Unlock()
	}()
	// Let the contender park on the mutex before the apply starts, so what the
	// test measures is the yield rather than a scheduling accident.
	time.Sleep(25 * time.Millisecond)

	var sawContenderMidApply atomic.Bool
	p.observePage = func(factPageStats) {
		if gotIn.Load() {
			sawContenderMidApply.Store(true)
		}
	}

	var yield applyYield
	if yielding {
		yield = func() bool {
			mutated, _ := yieldResolveMutex(g, mu)
			return mutated
		}
	}
	err := p.applyStagedFacts(context.Background(), g, "repo", spool, &semantic.EnrichResult{}, yield)
	mu.Unlock()
	contender.Wait()
	require.NoError(t, err)
	return sawContenderMidApply.Load()
}

// TestApplyReleasesTheResolveMutexToAQueuedResolver is the regression gate for
// the 24-minute silent stall.
//
// The tstypes apply held the store-wide resolve mutex from its first page to
// its last — measured at 1,088s on a 106k-node repository — and every other
// edge-mutating pass in the process queued behind it. The cross-repo resolver
// has always yielded between super-chunks; this asserts the apply does too.
func TestApplyReleasesTheResolveMutexToAQueuedResolver(t *testing.T) {
	require.True(t, applyWithContender(t, true, true),
		"a resolver waiting on the resolve mutex must get in before the apply finishes")
}

// TestApplyWithoutYieldHoldsTheMutexThroughout pins the behaviour the fix
// changed, so the test above cannot pass for an unrelated reason (a contender
// that was never actually blocked, say).
func TestApplyWithoutYieldHoldsTheMutexThroughout(t *testing.T) {
	require.False(t, applyWithContender(t, false, true),
		"without a yield the apply must keep the mutex for its whole run")
}

// TestApplyDoesNotYieldToAnUnqueuedContender is the gate for the regression the
// first version of the yield shipped.
//
// Enrichment applies do not publish their waits: they serialise correctly by
// simply holding the mutex, and each one owns a per-pass read-through cache
// that a release invalidates. An apply that yielded to whoever happened to be
// blocked let its siblings in at every page, and measured 2026-09-02 the three
// concurrent applies that resulted ran 2.7x slower for identical work (89.8% ->
// 61.0% node cache hit rate) until all three overran their budgets. Only a pass
// that registered in the resolve queue is worth the cache.
func TestApplyDoesNotYieldToAnUnqueuedContender(t *testing.T) {
	require.False(t, applyWithContender(t, true, false),
		"the apply must not release the mutex for a pass that never queued for it")
}
