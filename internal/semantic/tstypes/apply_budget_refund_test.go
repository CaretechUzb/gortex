package tstypes

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/graph"
)

// TestEnrichRepoContextRefundsALongYieldToTheBudget is the end-to-end gate for
// the second half of the yield fix.
//
// The apply yields the store-wide resolve mutex at page boundaries so a queued
// resolver is not stuck behind a 13-minute pass. A resolver admitted that way
// can itself run for minutes, and while it does, this pass is not working — it
// is queued, exactly as it was before its first mu.Lock(), which
// EnrichRepoContext already refunds. Charging the second wait and not the first
// is what turned three concurrent applies into three Partials on 2026-09-02.
//
// The fixture makes the wait longer than the whole budget on purpose: without
// the refund the pass cannot survive it, so a green run is evidence and not a
// coincidence.
func TestEnrichRepoContextRefundsALongYieldToTheBudget(t *testing.T) {
	const (
		budget = 1500 * time.Millisecond
		hold   = 2500 * time.Millisecond
	)

	// Two spool pages' worth, so the apply reaches a page boundary — the only
	// place it can yield.
	files := make(map[string]string, 3*tstypesFactPageFiles)
	for i := 0; i < 3*tstypesFactPageFiles; i++ {
		files[fmt.Sprintf("src/f%05d.py", i)] = fmt.Sprintf(`
class Base:
    def run(self):
        return None

class T%d(Base):
    def run(self):
        return Base()
`, i)
	}
	g, root := buildFixture(t, files)

	// The production threshold is 5s; the fixture's hold is deliberately
	// shorter so the test does not sleep for it.
	restore := applyRefundLogThreshold
	applyRefundLogThreshold = 50 * time.Millisecond
	t.Cleanup(func() { applyRefundLogThreshold = restore })

	core, logs := observer.New(zap.InfoLevel)
	p := NewProvider(PythonSpec(), zap.New(core))

	mu := g.ResolveMutex()
	var contender sync.WaitGroup
	var once sync.Once
	// Fired from inside the apply, so the contender arrives while this pass
	// holds the mutex — the only arrangement that exercises the page-boundary
	// yield rather than the initial lock wait.
	p.observePage = func(factPageStats) {
		once.Do(func() {
			parked := make(chan struct{})
			contender.Add(1)
			go func() {
				defer contender.Done()
				id := graph.EnterResolveQueue("test resolver")
				close(parked)
				mu.Lock()
				graph.LeaveResolveQueue(id)
				time.Sleep(hold)
				mu.Unlock()
			}()
			<-parked
			// The register is published; give the goroutine its turn to block
			// on the mutex so the next yield sees a real waiter.
			time.Sleep(25 * time.Millisecond)
		})
	}

	res, err := p.EnrichRepoContext(context.Background(), g, "", root,
		func(int) time.Duration { return budget })
	contender.Wait()

	require.NoError(t, err)
	require.NotNil(t, res)
	require.False(t, res.Partial,
		"a pass must not be cut by time it spent waiting for the pass it yielded to (abort: %q)",
		res.AbortReason)

	refunds := logs.FilterMessage("tstypes: apply budget refunded for time queued behind another pass").All()
	require.Len(t, refunds, 1, "the refund must be reported, not silently applied")
	refunded, ok := refunds[0].ContextMap()["refunded"].(time.Duration)
	require.True(t, ok)
	require.GreaterOrEqual(t, refunded, hold/2,
		"the refund must cover the contender's hold, not a fraction of it")
}
