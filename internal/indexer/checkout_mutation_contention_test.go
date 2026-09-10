package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Admission contends only for the target checkout's own cycle lock. The shared
// view-build lane is not an admission stage: another checkout's build holding
// it does not delay an edit here (checkout_mutation_lane_test.go).
func TestCheckoutMutationContentionWaitsForAdmissionInsteadOfRefusingActiveWork(t *testing.T) {
	f, c, lifecycle := newCheckoutMutationFixture(t)
	before := f.route()
	c.cycleMu.Lock()
	release := c.cycleMu.Unlock
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
		t.Fatalf("mutation admission returned while the cycle lock was still held: %v", got.err)
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
}

func TestCheckoutMutationAdmissionPreservesCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, cause := range []error{context.Canceled} {
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

func TestCheckoutMutationContentionDeadlineReportsBusyAndReleasesAdmission(t *testing.T) {
	f, c, lifecycle := newCheckoutMutationFixture(t)
	before := f.route()
	path := filepath.Join(f.worktree, "helper.go")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c.cycleMu.Lock()
	release := c.cycleMu.Unlock
	defer func() {
		if release != nil {
			release()
		}
	}()
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	lease, err := lifecycle.BeginCheckoutMutation(ctx, f.checkoutID, f.worktree, before.RouteEpoch)
	if lease != nil {
		lease.Close()
		t.Fatal("admitted mutation while the cycle lock was held")
	}
	if !errors.Is(err, ErrCheckoutMutationBusy) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contention deadline lost busy/deadline identity: %v", err)
	}
	if !strings.Contains(err.Error(), "lane is busy; retry") || !strings.Contains(err.Error(), "checkout cycle lock") {
		t.Fatalf("contention deadline lost actionable stage diagnosis: %v", err)
	}
	c.mu.Lock()
	active := c.sourceMutations
	c.mu.Unlock()
	stats := c.gate.Stats()
	if active != 0 || stats.InteractiveQueued != 0 || stats.Active {
		t.Fatalf("canceled admission leaked resources: mutations=%d gate=%+v", active, stats)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(original) || f.route() != before {
		t.Fatalf("failed admission changed source or route: %v", err)
	}
	release()
	release = nil
	lease, err = lifecycle.BeginCheckoutMutation(t.Context(), f.checkoutID, f.worktree, before.RouteEpoch)
	if err != nil {
		t.Fatalf("retry after contention released failed: %v", err)
	}
	lease.Close()
	if c.gate.Stats().Active || f.route() != before {
		t.Fatal("retry dry run leaked gate or changed route")
	}
}
