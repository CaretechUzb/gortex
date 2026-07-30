package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"sync"
)

const repositoryMutationPathCap = 2048

var errRepositoryMutationCoordinatorClosed = errors.New("repository mutation coordinator is closed")

type repositoryMutationExecutor func(paths []string) (*IndexResult, error)

type repositoryMutationScope struct {
	full  bool
	paths map[string]struct{}
}

func (s *repositoryMutationScope) merge(paths []string) {
	if s.full {
		return
	}
	if len(paths) == 0 {
		s.full = true
		s.paths = nil
		return
	}
	if s.paths == nil {
		s.paths = make(map[string]struct{}, len(paths))
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		s.paths[filepath.Clean(path)] = struct{}{}
		if len(s.paths) > repositoryMutationPathCap {
			s.full = true
			s.paths = nil
			return
		}
	}
	if len(s.paths) == 0 {
		s.full = true
		s.paths = nil
	}
}

func (s *repositoryMutationScope) take() []string {
	if s.full {
		s.full = false
		s.paths = nil
		return nil
	}
	paths := make([]string, 0, len(s.paths))
	for path := range s.paths {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	s.paths = nil
	return paths
}

type repositoryMutationOutcome struct {
	result *IndexResult
	err    error
}

type repositoryMutationWaiter struct {
	generation uint64
	done       chan repositoryMutationOutcome
}

// repositoryMutationCoordinator is the single mutation lane for one Indexer.
// Reconcile requests coalesce while queued: path scopes union, a full request
// dominates, and requests admitted during execution advance the dirty
// generation and force another pass before the worker becomes idle. Point
// mutations use runExclusive and share the same lane without being folded
// together, preserving watcher receipts and callbacks.
type repositoryMutationCoordinator struct {
	mu sync.Mutex

	lane chan struct{}
	work sync.WaitGroup

	executor repositoryMutationExecutor
	closed   bool
	running  bool

	requestedGeneration uint64
	completedGeneration uint64
	pending             repositoryMutationScope
	waiters             []*repositoryMutationWaiter
}

func newRepositoryMutationCoordinator(executor repositoryMutationExecutor) *repositoryMutationCoordinator {
	lane := make(chan struct{}, 1)
	lane <- struct{}{}
	return &repositoryMutationCoordinator{lane: lane, executor: executor}
}

func (c *repositoryMutationCoordinator) setExecutor(executor repositoryMutationExecutor) error {
	if executor == nil {
		return errors.New("repository mutation executor is nil")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errRepositoryMutationCoordinatorClosed
	}
	if c.running || c.requestedGeneration != c.completedGeneration {
		return errors.New("repository mutation executor cannot change while work is pending")
	}
	c.executor = executor
	return nil
}

func (c *repositoryMutationCoordinator) reconcile(
	ctx context.Context,
	paths []string,
) (*IndexResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	waiter := &repositoryMutationWaiter{done: make(chan repositoryMutationOutcome, 1)}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errRepositoryMutationCoordinatorClosed
	}
	c.requestedGeneration++
	waiter.generation = c.requestedGeneration
	c.pending.merge(paths)
	c.waiters = append(c.waiters, waiter)
	if !c.running {
		c.running = true
		c.work.Add(1)
		go c.drain()
	}
	c.mu.Unlock()

	select {
	case outcome := <-waiter.done:
		return outcome.result, outcome.err
	case <-ctx.Done():
		// Admission is durable. Cancellation stops this caller waiting but does
		// not abandon disk reconciliation or the dirty follow-up generation.
		return nil, ctx.Err()
	}
}

func (c *repositoryMutationCoordinator) drain() {
	defer c.work.Done()
	for {
		// Acquire the mutation lane before snapshotting pending scope. Requests
		// admitted while a point mutation owns the lane therefore coalesce into
		// one batch instead of being split merely because the worker was queued.
		<-c.lane
		c.mu.Lock()
		if len(c.waiters) == 0 {
			c.running = false
			c.mu.Unlock()
			c.lane <- struct{}{}
			return
		}
		generation := c.requestedGeneration
		paths := c.pending.take()
		waiters := c.waiters
		c.waiters = nil
		executor := c.executor
		c.mu.Unlock()

		var outcome repositoryMutationOutcome
		if executor == nil {
			outcome.err = errors.New("repository mutation executor is not configured")
		} else {
			outcome.result, outcome.err = executor(paths)
		}
		c.lane <- struct{}{}

		c.mu.Lock()
		if generation > c.completedGeneration {
			c.completedGeneration = generation
		}
		c.mu.Unlock()
		for _, waiter := range waiters {
			waiter.done <- outcome
		}
	}
}

func (c *repositoryMutationCoordinator) runExclusive(ctx context.Context, fn func() error) error {
	if fn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return errRepositoryMutationCoordinatorClosed
	}
	c.work.Add(1)
	c.mu.Unlock()
	defer c.work.Done()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.lane:
	}
	defer func() { c.lane <- struct{}{} }()
	return fn()
}

func (c *repositoryMutationCoordinator) closeAndWait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()

	done := make(chan struct{})
	go func() {
		c.work.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type repositoryMutationCoordinatorStats struct {
	RequestedGeneration uint64
	CompletedGeneration uint64
	Running             bool
	PendingPaths        int
	PendingFull         bool
}

func (c *repositoryMutationCoordinator) stats() repositoryMutationCoordinatorStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return repositoryMutationCoordinatorStats{
		RequestedGeneration: c.requestedGeneration,
		CompletedGeneration: c.completedGeneration,
		Running:             c.running,
		PendingPaths:        len(c.pending.paths),
		PendingFull:         c.pending.full,
	}
}

func (idx *Indexer) repositoryMutations() *repositoryMutationCoordinator {
	idx.repositoryMutationOnce.Do(func() {
		idx.repositoryMutation = newRepositoryMutationCoordinator(func(paths []string) (*IndexResult, error) {
			return idx.incrementalReindexWatcherPaths(idx.rootPath, paths)
		})
	})
	return idx.repositoryMutation
}

func (idx *Indexer) setRepositoryMutationExecutor(executor repositoryMutationExecutor) error {
	return idx.repositoryMutations().setExecutor(executor)
}

func (idx *Indexer) coordinateRepositoryReindex(
	ctx context.Context,
	paths []string,
) (*IndexResult, error) {
	return idx.repositoryMutations().reconcile(ctx, paths)
}

// CoordinateRepositoryMutation serializes one non-coalescible repository
// mutation with every watcher, reconciliation, and janitor pipeline for this
// Indexer. The callback must use raw mutation methods and must not submit back
// into the coordinator.
func (idx *Indexer) CoordinateRepositoryMutation(ctx context.Context, fn func() error) error {
	return idx.repositoryMutations().runExclusive(ctx, fn)
}

func (idx *Indexer) closeRepositoryMutations(ctx context.Context) error {
	return idx.repositoryMutations().closeAndWait(ctx)
}
