package indexer

import (
	"context"
	"errors"
	"testing"
	"time"
)

type indexPhaseBeginResult struct {
	lease *indexPhaseLease
	err   error
}

func beginIndexPhaseAsync(ctx context.Context, reservation *indexPhaseReservation) <-chan indexPhaseBeginResult {
	result := make(chan indexPhaseBeginResult, 1)
	go func() {
		lease, err := reservation.Begin(ctx)
		result <- indexPhaseBeginResult{lease: lease, err: err}
	}()
	return result
}

func awaitIndexPhase(t *testing.T, result <-chan indexPhaseBeginResult) *indexPhaseLease {
	t.Helper()
	select {
	case completed := <-result:
		if completed.err != nil {
			t.Fatalf("begin index phase: %v", completed.err)
		}
		if completed.lease == nil {
			t.Fatal("begin index phase returned nil lease")
		}
		return completed.lease
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for index phase")
		return nil
	}
}

func awaitReservationReady(t *testing.T, reservation *indexPhaseReservation) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		reservation.coordinator.mu.Lock()
		ready := reservation.ready
		reservation.coordinator.mu.Unlock()
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for phase reservation readiness")
		}
		time.Sleep(time.Millisecond)
	}
}

func assertReservationNotGranted(t *testing.T, reservation *indexPhaseReservation) {
	t.Helper()
	reservation.coordinator.mu.Lock()
	granted := reservation.granted
	reservation.coordinator.mu.Unlock()
	if granted {
		t.Fatalf("phase reservation %d was granted out of order", reservation.sequence)
	}
}

func TestIndexPhaseCoordinatorFreezesExistingDrainsBeforeOneQueuedDirect(t *testing.T) {
	coordinator := newIndexPhaseCoordinator()

	ownerReservation := coordinator.reserveDirect(1)
	owner, err := ownerReservation.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin owner: %v", err)
	}
	queuedDirect := coordinator.reserveDirect(1)
	drainOne := coordinator.reserveShadowDrain(true)
	drainTwo := coordinator.reserveShadowDrain(true)

	// Readiness does not relax reservation FIFO: drain two cannot pass an
	// earlier frozen drain that is still finishing its post-parse tail.
	drainTwoResult := beginIndexPhaseAsync(context.Background(), drainTwo)
	awaitReservationReady(t, drainTwo)
	owner.Release()
	if snapshot := coordinator.snapshot(); !snapshot.priorityCycle || snapshot.activeKind != 0 {
		t.Fatalf("release did not freeze an idle handoff: %+v", snapshot)
	}

	directResult := beginIndexPhaseAsync(context.Background(), queuedDirect)
	awaitReservationReady(t, queuedDirect)
	lateDrain := coordinator.reserveShadowDrain(true)
	lateDrainResult := beginIndexPhaseAsync(context.Background(), lateDrain)
	awaitReservationReady(t, lateDrain)
	assertReservationNotGranted(t, queuedDirect)
	assertReservationNotGranted(t, drainTwo)
	assertReservationNotGranted(t, lateDrain)

	drainOneResult := beginIndexPhaseAsync(context.Background(), drainOne)
	firstLease := awaitIndexPhase(t, drainOneResult)
	assertReservationNotGranted(t, drainTwo)
	assertReservationNotGranted(t, queuedDirect)
	assertReservationNotGranted(t, lateDrain)

	firstLease.Release()
	secondLease := awaitIndexPhase(t, drainTwoResult)
	assertReservationNotGranted(t, queuedDirect)
	assertReservationNotGranted(t, lateDrain)

	secondLease.Release()
	directLease := awaitIndexPhase(t, directResult)
	assertReservationNotGranted(t, lateDrain)

	directLease.Release()
	lateLease := awaitIndexPhase(t, lateDrainResult)
	lateLease.Release()

	if snapshot := coordinator.snapshot(); snapshot.activeKind != 0 || snapshot.directQueued != 0 || snapshot.drainQueued != 0 || snapshot.priorityCycle {
		t.Fatalf("coordinator did not return to idle: %+v", snapshot)
	}
}

func TestIndexPhaseCoordinatorCancellationAdvancesFrozenHandoff(t *testing.T) {
	coordinator := newIndexPhaseCoordinator()
	owner, err := coordinator.reserveDirect(1).Begin(context.Background())
	if err != nil {
		t.Fatalf("begin owner: %v", err)
	}
	queuedDirect := coordinator.reserveDirect(1)
	firstDrain := coordinator.reserveShadowDrain(true)
	secondDrain := coordinator.reserveShadowDrain(true)
	secondResult := beginIndexPhaseAsync(context.Background(), secondDrain)
	awaitReservationReady(t, secondDrain)

	directContext, cancelDirect := context.WithCancel(context.Background())
	directResult := beginIndexPhaseAsync(directContext, queuedDirect)
	awaitReservationReady(t, queuedDirect)
	owner.Release()
	firstDrain.Cancel()
	secondLease := awaitIndexPhase(t, secondResult)

	cancelDirect()
	select {
	case completed := <-directResult:
		if !errors.Is(completed.err, context.Canceled) {
			t.Fatalf("queued direct cancellation error = %v", completed.err)
		}
		if completed.lease != nil {
			t.Fatal("canceled queued direct returned a lease")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canceled direct")
	}

	lateDrain := coordinator.reserveShadowDrain(true)
	lateResult := beginIndexPhaseAsync(context.Background(), lateDrain)
	awaitReservationReady(t, lateDrain)
	secondLease.Release()
	lateLease := awaitIndexPhase(t, lateResult)
	lateLease.Release()

	if snapshot := coordinator.snapshot(); snapshot.activeKind != 0 || snapshot.directQueued != 0 || snapshot.drainQueued != 0 || snapshot.priorityCycle {
		t.Fatalf("coordinator did not recover after cancellation: %+v", snapshot)
	}
}

func TestLargeDirectParsePhaseReleasesPhaseBeforeAdmission(t *testing.T) {
	coordinator := newIndexPhaseCoordinator()
	budget := newLargeDirectAdmissionBudget(1)
	first, err := acquireLargeDirectParsePhase(context.Background(), budget, coordinator, 1)
	if err != nil {
		t.Fatalf("acquire first direct phase: %v", err)
	}

	type directResult struct {
		lease *largeDirectParsePhaseLease
		err   error
	}
	secondResult := make(chan directResult, 1)
	go func() {
		lease, acquireErr := acquireLargeDirectParsePhase(context.Background(), budget, coordinator, 1)
		secondResult <- directResult{lease: lease, err: acquireErr}
	}()

	deadline := time.Now().Add(5 * time.Second)
	for budget.snapshot().waiters != 1 || coordinator.snapshot().directQueued != 1 {
		if time.Now().After(deadline) {
			t.Fatal("second direct parse did not queue behind admission")
		}
		time.Sleep(time.Millisecond)
	}

	drain := coordinator.reserveShadowDrain(true)
	drainResult := beginIndexPhaseAsync(context.Background(), drain)
	awaitReservationReady(t, drain)
	first.Release()
	drainLease := awaitIndexPhase(t, drainResult)

	select {
	case completed := <-secondResult:
		if completed.lease != nil {
			completed.lease.Release()
		}
		t.Fatalf("second direct passed pressure drain: %v", completed.err)
	default:
	}

	drainLease.Release()
	select {
	case completed := <-secondResult:
		if completed.err != nil {
			t.Fatalf("acquire second direct phase: %v", completed.err)
		}
		if completed.lease == nil {
			t.Fatal("second direct returned nil lease")
		}
		completed.lease.Release()
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for second direct phase")
	}

	stats := budget.snapshot()
	if stats.used != 0 || stats.waiters != 0 {
		t.Fatalf("large-direct admission leaked: %+v", stats)
	}
}

func TestLargeDirectParsePhaseCancellationWithdrawsIntent(t *testing.T) {
	coordinator := newIndexPhaseCoordinator()
	budget := newLargeDirectAdmissionBudget(1)
	first, err := acquireLargeDirectParsePhase(context.Background(), budget, coordinator, 1)
	if err != nil {
		t.Fatalf("acquire first direct phase: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, acquireErr := acquireLargeDirectParsePhase(ctx, budget, coordinator, 1)
		result <- acquireErr
	}()
	deadline := time.Now().Add(5 * time.Second)
	for budget.snapshot().waiters != 1 {
		if time.Now().After(deadline) {
			t.Fatal("second direct parse did not enter admission wait")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case acquireErr := <-result:
		if !errors.Is(acquireErr, context.Canceled) {
			t.Fatalf("canceled direct acquire error = %v", acquireErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canceled direct acquire")
	}
	first.Release()

	if snapshot := coordinator.snapshot(); snapshot.activeKind != 0 || snapshot.directQueued != 0 || snapshot.drainQueued != 0 || snapshot.priorityCycle {
		t.Fatalf("canceled direct intent leaked: %+v", snapshot)
	}
}

func TestIndexPhaseCoordinatorOrdinaryWorkBypasses(t *testing.T) {
	coordinator := newIndexPhaseCoordinator()
	if reservation := coordinator.reserveShadowDrain(false); reservation != nil {
		t.Fatal("ordinary shadow drain entered phase coordinator")
	}
	budget := newLargeDirectAdmissionBudget(1)
	lease, err := acquireLargeDirectParsePhase(context.Background(), budget, coordinator, 0)
	if err != nil {
		t.Fatalf("ordinary direct acquire: %v", err)
	}
	if lease != nil {
		t.Fatal("ordinary direct parse entered phase coordinator")
	}
	if snapshot := coordinator.snapshot(); snapshot.activeKind != 0 || snapshot.directQueued != 0 || snapshot.drainQueued != 0 {
		t.Fatalf("ordinary work changed coordinator state: %+v", snapshot)
	}
}
