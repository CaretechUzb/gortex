package indexer

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRepositoryMutationBatchAdmissionPrecedesLane(t *testing.T) {
	coordinator := newRepositoryMutationCoordinator(nil)
	var batchGate sync.RWMutex
	coordinator.batchMutationGate = &batchGate

	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- coordinator.runExclusive(context.Background(), func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	select {
	case <-firstEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("first mutation did not enter")
	}

	secondAdmitted := make(chan struct{})
	coordinator.batchAdmissionHook = sync.OnceFunc(func() { close(secondAdmitted) })
	secondEntered := make(chan struct{})
	var secondEnteredLane atomic.Bool
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- coordinator.runExclusive(context.Background(), func() error {
			secondEnteredLane.Store(true)
			close(secondEntered)
			return nil
		})
	}()
	// Wait for the queued mutation to own its batch read admission while it is
	// still waiting for the first mutation's repository lane. This deterministic
	// point is the ordering guarantee the regression exercises.
	select {
	case <-secondAdmitted:
	case <-time.After(2 * time.Second):
		t.Fatal("queued mutation was not admitted before its repository lane")
	}

	writerOvertook := make(chan bool, 1)
	writerDone := make(chan error, 1)
	go func() {
		batchGate.Lock()
		writerOvertook <- !secondEnteredLane.Load()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := coordinator.runExclusiveLaneOnly(ctx, func() error { return nil })
		batchGate.Unlock()
		writerDone <- err
	}()

	close(releaseFirst)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first mutation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first mutation did not finish")
	}
	select {
	case <-secondEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("queued mutation deadlocked with batch writer")
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("second mutation: %v", err)
	}
	if <-writerOvertook {
		t.Fatal("batch writer overtook the already-admitted queued mutation")
	}
	select {
	case err := <-writerDone:
		if err != nil {
			t.Fatalf("batch writer lane: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("batch writer did not finish after queued mutation")
	}
}
