package tstypes

import (
	"context"
	"sync"
	"time"
)

// applyRefundLogThreshold is the refund above which the pass says so. The same
// 5s the "apply began after queueing" line uses, so the two describe one
// phenomenon at one resolution. A var only so a test can shorten it; nothing at
// runtime writes it.
var applyRefundLogThreshold = 5 * time.Second

// applyContextErr reports why the apply context is done, preferring its cause.
//
// The budget expires by cancelling with context.DeadlineExceeded as the cause,
// so a caller reading Err() alone would record every budget expiry as "context
// canceled" — indistinguishable from a daemon shutdown, and specifically the
// string the futile-pass detector keys on to tell an exhausted budget from an
// interrupted one. Cause also composes upward: a parent whose own deadline
// elapsed reports DeadlineExceeded here without special-casing.
func applyContextErr(ctx context.Context) error {
	if ctx == nil || ctx.Err() == nil {
		return nil
	}
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return ctx.Err()
}

// applyBudget is the apply phase's deadline expressed as a timer that can be
// PAUSED, so time this pass spends waiting for another pass is not charged to
// it.
//
// EnrichRepoContext already refuses to charge the wait it can see: the initial
// mu.Lock() before the apply is measured and added back to deadlineAt, with a
// comment recording what happens otherwise — "a 641s pass that was ~630s of
// queueing behind a Go compiler apply". The page-boundary yield opened a second
// door into the same failure. Its re-acquire is a wait of exactly the same kind
// and, being inside the apply, a plain context.WithDeadline charged every
// millisecond of it. Measured 2026-09-02: three concurrent applies, each yielding
// to the others, all three burned their full budget and returned Partial.
//
// A context deadline cannot be moved once armed, which is why this is a timer
// and a cancel rather than a WithDeadline. Pausing rather than refunding
// afterwards is deliberate: a resolver pass admitted by one yield can run for
// minutes, and a refund applied after the fact arrives too late for a timer that
// already fired mid-wait.
type applyBudget struct {
	mu        sync.Mutex
	timer     *time.Timer
	remaining time.Duration
	started   time.Time
	pausedAt  time.Time
	refund    time.Duration
	running   bool
	expired   bool
}

// newApplyBudget arms a budget of d that calls expire once d elapses of
// UNPAUSED time. Returns nil for a non-positive budget — an unbounded pass has
// nothing to protect, and every method below is nil-safe so the caller needs no
// branch.
func newApplyBudget(d time.Duration, expire func()) *applyBudget {
	if d <= 0 || expire == nil {
		return nil
	}
	b := &applyBudget{remaining: d, started: time.Now(), running: true}
	b.timer = time.AfterFunc(d, func() {
		b.mu.Lock()
		b.expired = true
		b.running = false
		b.mu.Unlock()
		// Outside the lock: expire cancels a context, whose listeners must
		// never be able to re-enter this budget and deadlock it.
		expire()
	})
	return b
}

// pause stops the clock. Safe to call when already paused or expired.
func (b *applyBudget) pause() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.running || b.expired {
		return
	}
	b.timer.Stop()
	b.remaining -= time.Since(b.started)
	b.running = false
	b.pausedAt = time.Now()
}

// resume restarts the clock with whatever was left, and records the paused
// span as refunded. A budget that expired before it was paused stays expired:
// re-arming there would hand back time the pass had already overrun.
func (b *applyBudget) resume() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running || b.expired {
		return
	}
	b.refund += time.Since(b.pausedAt)
	if b.remaining <= 0 {
		// Exhausted exactly at the pause. Arm the smallest positive interval
		// rather than 0, which time.Timer treats as "fire now" from a
		// goroutine the caller cannot observe deterministically.
		b.remaining = time.Nanosecond
	}
	b.started = time.Now()
	b.running = true
	b.timer.Reset(b.remaining)
}

// stop disarms the budget for good. Idempotent; the caller defers it.
func (b *applyBudget) stop() {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.timer != nil {
		b.timer.Stop()
	}
	b.running = false
}

// refunded is the total time the budget spent paused: the wait this pass did
// for other passes and was not charged for.
func (b *applyBudget) refunded() time.Duration {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.refund
}
