package indexer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestCheckoutMutationContentionReportsAdmissionStage(t *testing.T) {
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
			lease, err := lifecycle.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
			if lease != nil {
				lease.Close()
				t.Fatal("busy checkout admitted a mutation")
			}
			if !errors.Is(err, ErrCheckoutMutationBusy) || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("lost busy/deadline identity: %v", err)
			}
			if !strings.Contains(err.Error(), stage) || !strings.Contains(err.Error(), "250ms") {
				t.Errorf("admission error omits stage or budget: %v", err)
			}
			if f.route() != before {
				t.Fatal("refusal changed the route")
			}
			c.mu.Lock()
			active := c.sourceMutations
			c.mu.Unlock()
			if active != 0 {
				t.Fatalf("refusal leaked %d source mutation leases", active)
			}
			release()
			release = nil
			lease, err = lifecycle.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
			if err != nil {
				t.Fatalf("admission did not recover after release: %v", err)
			}
			lease.Close()
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
