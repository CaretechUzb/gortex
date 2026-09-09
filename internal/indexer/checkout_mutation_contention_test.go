package indexer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestCheckoutMutationContentionWaitsForAdmissionInsteadOfRefusingActiveWork(t *testing.T) {
	for _, stage := range []string{"shared view-build gate", "checkout cycle lock"} {
		t.Run(stage, func(t *testing.T) {
			f, c, lifecycle := newCheckoutMutationFixture(t)
			before := f.route()
			var release func()
			if stage == "shared view-build gate" {
				// The daemon shares this gate across checkout/ref builders. No
				// source mutation or cycle lock on the target checkout is needed.
				var err error
				release, err = c.gate.Acquire(t.Context(), ViewBuildBackground)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				c.cycleMu.Lock()
				release = c.cycleMu.Unlock
			}
			defer func() {
				if release != nil {
					release()
				}
			}()

			type result struct {
				lease *CheckoutMutation
				err   error
			}
			resultc := make(chan result, 1)
			go func() {
				lease, err := lifecycle.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
				resultc <- result{lease: lease, err: err}
			}()

			select {
			case got := <-resultc:
				if got.lease != nil {
					got.lease.Close()
				}
				t.Fatalf("mutation admission returned while %s was still held: %v", stage, got.err)
			case <-time.After(2 * 250 * time.Millisecond):
			}

			if f.route() != before {
				t.Fatal("waiting admission changed the route")
			}
			c.mu.Lock()
			active := c.sourceMutations
			c.mu.Unlock()
			if active != 1 {
				t.Fatalf("waiting admission should hold one tracked source mutation, got %d", active)
			}

			release()
			release = nil

			select {
			case got := <-resultc:
				if got.err != nil {
					t.Fatalf("admission did not recover after release: %v", got.err)
				}
				got.lease.Close()
			case <-time.After(5 * time.Second):
				t.Fatal("mutation admission did not complete after contention released")
			}
			if f.route() != before || c.gate.Stats().Active {
				t.Fatal("dry-run admission leaked gate or changed route")
			}
		})
	}
}

func TestCheckoutMutationAdmissionPreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, cause := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := checkoutMutationAdmissionError(ctx, "shared view-build gate", cause); got != cause {
			t.Fatalf("caller cancellation was relabeled as contention: %v", got)
		}
	}
	if got := checkoutMutationAdmissionError(t.Context(), "shared view-build gate", ErrCheckoutMutationStale); got != ErrCheckoutMutationStale {
		t.Fatalf("non-timeout admission error was replaced: %v", got)
	}
}

func TestCheckoutMutationAdmissionReportsQueueFullAsBusy(t *testing.T) {
	got := checkoutMutationAdmissionError(t.Context(), "shared view-build gate", &ViewBuildQueueFullError{
		Priority: ViewBuildInteractive,
		Limit:    1,
	})
	if !errors.Is(got, ErrCheckoutMutationBusy) || !errors.Is(got, ErrViewBuildQueueFull) {
		t.Fatalf("queue-full admission did not preserve busy/queue identity: %v", got)
	}
	if !strings.Contains(got.Error(), "shared view-build gate") {
		t.Fatalf("queue-full admission error omits stage: %v", got)
	}
}
