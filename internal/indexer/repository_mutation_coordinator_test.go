package indexer

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func waitForRepositoryMutationStats(
	t *testing.T,
	coordinator *repositoryMutationCoordinator,
	predicate func(repositoryMutationCoordinatorStats) bool,
) repositoryMutationCoordinatorStats {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		stats := coordinator.stats()
		if predicate(stats) {
			return stats
		}
		time.Sleep(time.Millisecond)
	}
	stats := coordinator.stats()
	t.Fatalf("coordinator state did not converge: %+v", stats)
	return stats
}

func TestRepositoryMutationScopeUnionAndFullEscalation(t *testing.T) {
	var scope repositoryMutationScope
	scope.merge([]string{"b.go", "a.go", "a.go"})
	if got, want := scope.take(), []string{"a.go", "b.go"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("union = %#v, want %#v", got, want)
	}

	scope.merge([]string{"one.go"})
	scope.merge(nil)
	if got := scope.take(); got != nil {
		t.Fatalf("explicit full scope = %#v, want nil", got)
	}

	paths := make([]string, repositoryMutationPathCap+1)
	for i := range paths {
		paths[i] = time.Unix(int64(i), 0).Format("20060102150405.000000000")
	}
	scope.merge(paths)
	if got := scope.take(); got != nil {
		t.Fatalf("cap-escalated scope retained %d paths", len(got))
	}
}

func TestRepositoryMutationCoordinatorUnionsDirtyFollowup(t *testing.T) {
	var mu sync.Mutex
	var calls [][]string
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		mu.Lock()
		call := len(calls) + 1
		calls = append(calls, append([]string(nil), paths...))
		mu.Unlock()
		if call == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		return &IndexResult{FileCount: call}, nil
	})

	type response struct {
		result *IndexResult
		err    error
	}
	responses := make(chan response, 3)
	go func() {
		result, err := coordinator.reconcile(context.Background(), []string{"a.go"})
		responses <- response{result: result, err: err}
	}()
	<-firstStarted
	go func() {
		result, err := coordinator.reconcile(context.Background(), []string{"c.go"})
		responses <- response{result: result, err: err}
	}()
	go func() {
		result, err := coordinator.reconcile(context.Background(), []string{"b.go", "c.go"})
		responses <- response{result: result, err: err}
	}()
	waitForRepositoryMutationStats(t, coordinator, func(stats repositoryMutationCoordinatorStats) bool {
		return stats.RequestedGeneration == 3 && stats.PendingPaths == 2
	})
	close(releaseFirst)

	for range 3 {
		response := <-responses
		if response.err != nil || response.result == nil {
			t.Fatalf("response = result:%#v err:%v", response.result, response.err)
		}
	}
	if err := coordinator.closeAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if got, want := calls, [][]string{{"a.go"}, {"b.go", "c.go"}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("executions = %#v, want %#v", got, want)
	}
}

func TestRepositoryMutationCoordinatorFullDominatesQueuedPaths(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	var gotPaths []string
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		calls.Add(1)
		gotPaths = append([]string(nil), paths...)
		return &IndexResult{}, nil
	})
	exclusiveDone := make(chan error, 1)
	go func() {
		exclusiveDone <- coordinator.runExclusive(context.Background(), func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered

	results := make(chan error, 2)
	go func() {
		_, err := coordinator.reconcile(context.Background(), []string{"one.go"})
		results <- err
	}()
	go func() {
		_, err := coordinator.reconcile(context.Background(), nil)
		results <- err
	}()
	waitForRepositoryMutationStats(t, coordinator, func(stats repositoryMutationCoordinatorStats) bool {
		return stats.RequestedGeneration == 2 && stats.PendingFull
	})
	close(release)
	if err := <-exclusiveDone; err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 1 || gotPaths != nil {
		t.Fatalf("full execution = calls:%d paths:%#v, want 1/nil", calls.Load(), gotPaths)
	}
}

func TestRepositoryMutationCoordinatorSerializesMixedWork(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	enter := func() {
		now := active.Add(1)
		for {
			old := maximum.Load()
			if now <= old || maximum.CompareAndSwap(old, now) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
	}
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		enter()
		return &IndexResult{}, nil
	})

	var wait sync.WaitGroup
	for i := 0; i < 32; i++ {
		wait.Add(2)
		go func() {
			defer wait.Done()
			_, _ = coordinator.reconcile(context.Background(), []string{"same.go"})
		}()
		go func() {
			defer wait.Done()
			_ = coordinator.runExclusive(context.Background(), func() error {
				enter()
				return nil
			})
		}()
	}
	wait.Wait()
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent mutation pipelines = %d, want 1", maximum.Load())
	}
}

func TestRepositoryMutationCancellationDoesNotCancelAdmittedWork(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	finished := make(chan struct{})
	coordinator := newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
		close(started)
		<-release
		close(finished)
		return &IndexResult{}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result, err := coordinator.reconcile(ctx, []string{"a.go"})
	if result != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("canceled wait = result:%#v err:%v", result, err)
	}
	<-started
	close(release)
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("admitted mutation was canceled with its waiter")
	}
	if err := coordinator.closeAndWait(context.Background()); err != nil {
		t.Fatal(err)
	}
	stats := coordinator.stats()
	if stats.CompletedGeneration != 1 || stats.Running {
		t.Fatalf("completed state = %+v", stats)
	}
	if _, err := coordinator.reconcile(context.Background(), nil); !errors.Is(err, errRepositoryMutationCoordinatorClosed) {
		t.Fatalf("closed reconcile error = %v", err)
	}
	if err := coordinator.runExclusive(context.Background(), func() error { return nil }); !errors.Is(err, errRepositoryMutationCoordinatorClosed) {
		t.Fatalf("closed exclusive error = %v", err)
	}
}
