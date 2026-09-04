package tstypes

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// expiries counts budget expiries and lets a test wait for one without a sleep
// race: the timer fires on its own goroutine, so polling a bool would be a flake
// waiting to happen.
type expiries struct {
	n    atomic.Int64
	fire chan struct{}
}

func newExpiries() *expiries {
	return &expiries{fire: make(chan struct{}, 4)}
}

func (e *expiries) record() {
	e.n.Add(1)
	select {
	case e.fire <- struct{}{}:
	default:
	}
}

func (e *expiries) waitFor(t *testing.T, d time.Duration) bool {
	t.Helper()
	select {
	case <-e.fire:
		return true
	case <-time.After(d):
		return false
	}
}

func TestApplyBudgetExpiresOnUnpausedTime(t *testing.T) {
	e := newExpiries()
	b := newApplyBudget(40*time.Millisecond, e.record)
	require.NotNil(t, b)
	defer b.stop()

	require.True(t, e.waitFor(t, 2*time.Second), "an unpaused budget must expire on schedule")
	require.EqualValues(t, 1, e.n.Load())
}

// TestApplyBudgetDoesNotChargePausedTime is the invariant the whole type exists
// for: time this pass spends waiting for another pass is not its own.
//
// Without it, a yield that admits a resolver pass running for minutes ends the
// apply mid-wait, and the apply reports a Partial it did not earn — which is
// how three concurrent applies each burned an 800s budget on 2026-09-02.
func TestApplyBudgetDoesNotChargePausedTime(t *testing.T) {
	e := newExpiries()
	b := newApplyBudget(120*time.Millisecond, e.record)
	require.NotNil(t, b)
	defer b.stop()

	// Spend a little of the budget, then park for well over what remains.
	time.Sleep(20 * time.Millisecond)
	b.pause()
	time.Sleep(300 * time.Millisecond)
	require.EqualValues(t, 0, e.n.Load(),
		"a paused budget must not expire while another pass holds the mutex")
	b.resume()

	require.GreaterOrEqual(t, b.refunded(), 250*time.Millisecond,
		"the paused span must be reported as refunded")
	require.True(t, e.waitFor(t, 2*time.Second),
		"the budget must still expire once its own clock runs out")
}

// TestApplyBudgetStaysExpiredAcrossAPause guards the other direction: a pass
// that already overran must not buy itself a second budget by yielding.
func TestApplyBudgetStaysExpiredAcrossAPause(t *testing.T) {
	e := newExpiries()
	b := newApplyBudget(30*time.Millisecond, e.record)
	require.NotNil(t, b)
	defer b.stop()

	require.True(t, e.waitFor(t, 2*time.Second))
	b.pause()
	b.resume()
	time.Sleep(80 * time.Millisecond)
	require.EqualValues(t, 1, e.n.Load(),
		"an expired budget must not re-arm when the pass yields again")
}

// TestApplyBudgetIsNilForAnUnboundedPass keeps every call site branch-free: a
// pass with no deadline policy gets a nil budget and must not panic on it.
func TestApplyBudgetIsNilForAnUnboundedPass(t *testing.T) {
	var b *applyBudget
	require.Nil(t, newApplyBudget(0, func() {}))
	require.Nil(t, newApplyBudget(time.Second, nil))
	b.pause()
	b.resume()
	b.stop()
	require.Zero(t, b.refunded())
}

// TestApplyContextErrPrefersTheCause is what keeps a budget expiry legible
// downstream: the futile-pass detector distinguishes an exhausted budget from an
// interrupted pass by the abort reason, and a cancel-with-cause reports
// "context canceled" through Err() alone.
func TestApplyContextErrPrefersTheCause(t *testing.T) {
	require.NoError(t, applyContextErr(context.Background()))
	require.NoError(t, applyContextErr(nil))

	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(context.DeadlineExceeded)
	require.Equal(t, context.DeadlineExceeded, applyContextErr(ctx))

	plain, cancelPlain := context.WithCancel(context.Background())
	cancelPlain()
	require.Equal(t, context.Canceled, applyContextErr(plain))
}
