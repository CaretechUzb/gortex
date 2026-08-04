package goanalysis

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestResolveLockSlicesReleaseBetweenSlices(t *testing.T) {
	var mu sync.Mutex
	slices := &resolveLockSlices{mu: &mu}

	firstDone := make(chan struct{})
	allowSecond := make(chan struct{})
	secondDone := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		if err := slices.with(context.Background(), func() error {
			time.Sleep(time.Millisecond)
			return nil
		}); err != nil {
			errCh <- err
			return
		}
		close(firstDone)
		<-allowSecond // representative compiler-only work outside the graph lock
		if err := slices.with(context.Background(), func() error {
			close(secondDone)
			return nil
		}); err != nil {
			errCh <- err
			return
		}
		errCh <- nil
	}()

	select {
	case <-firstDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first graph slice did not complete")
	}

	// A competing graph phase must be able to enter while the provider is
	// between bounded store slices.
	if !mu.TryLock() {
		t.Fatal("resolve mutex remained held between slices")
	}
	mu.Unlock()
	close(allowSecond)

	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second graph slice did not run")
	}
	if err := <-errCh; err != nil {
		t.Fatalf("slice failed: %v", err)
	}
	if slices.count != 2 {
		t.Fatalf("slice count = %d, want 2", slices.count)
	}
	if slices.held <= 0 {
		t.Fatalf("held duration = %v, want positive", slices.held)
	}
	if slices.maxHeld <= 0 || slices.maxHeld > slices.held {
		t.Fatalf("max held duration = %v, total held = %v", slices.maxHeld, slices.held)
	}
}

func TestResolveLockSlicesCancellationWhileWaiting(t *testing.T) {
	var mu sync.Mutex
	mu.Lock()
	slices := &resolveLockSlices{mu: &mu}
	ctx, cancel := context.WithCancel(context.Background())
	called := false
	errCh := make(chan error, 1)
	go func() {
		errCh <- slices.with(ctx, func() error {
			called = true
			return nil
		})
	}()

	// Let the waiter observe contention, then cancel without releasing the
	// mutex. lockResolveContext must abandon the wait and never run the slice.
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("wait error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled lock wait did not return")
	}
	if called {
		t.Fatal("cancelled graph slice executed")
	}
	if slices.count != 0 {
		t.Fatalf("successful slice count = %d, want 0", slices.count)
	}
	if slices.waited <= 0 {
		t.Fatalf("wait duration = %v, want positive", slices.waited)
	}

	mu.Unlock()
	if !mu.TryLock() {
		t.Fatal("cancelled waiter leaked the resolve mutex")
	}
	mu.Unlock()
}
