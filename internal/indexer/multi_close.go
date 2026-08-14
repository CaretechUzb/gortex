package indexer

import (
	"context"
	"errors"
)

var errMultiIndexerClosed = errors.New("multi-indexer is closed")

func (mi *MultiIndexer) isClosed() bool {
	if mi == nil {
		return true
	}
	mi.repositoryMutationMu.Lock()
	closed := mi.lifecycleClosed
	mi.repositoryMutationMu.Unlock()
	return closed
}

// Close rejects new repository mutations, drains every stable mutation lane,
// detaches the owned Indexers under mi.mu, and closes each Indexer outside that
// lock. Calls are idempotent; a context-canceled call may be retried to finish
// draining and teardown.
func (mi *MultiIndexer) Close(ctx context.Context) error {
	if mi == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}

	mi.closeMu.Lock()
	defer mi.closeMu.Unlock()
	if mi.lifecycleComplete {
		return nil
	}

	mi.repositoryMutationMu.Lock()
	mi.lifecycleClosed = true
	coordinators := make([]*repositoryMutationCoordinator, 0, len(mi.repositoryMutations))
	for _, coordinator := range mi.repositoryMutations {
		if coordinator != nil {
			coordinators = append(coordinators, coordinator)
		}
	}
	mi.repositoryMutationMu.Unlock()

	// Close every admission boundary before waiting on any one lane. This
	// prevents another repository from admitting work while Close is blocked on
	// an earlier in-flight mutation.
	for _, coordinator := range coordinators {
		coordinator.closeAdmission()
	}
	for _, coordinator := range coordinators {
		if err := coordinator.wait(ctx); err != nil {
			return err
		}
	}

	mi.mu.Lock()
	seen := make(map[*Indexer]struct{}, len(mi.indexers))
	indexers := make([]*Indexer, 0, len(mi.indexers))
	for _, idx := range mi.indexers {
		if idx == nil {
			continue
		}
		if _, exists := seen[idx]; exists {
			continue
		}
		seen[idx] = struct{}{}
		indexers = append(indexers, idx)
	}
	mi.indexers = make(map[string]*Indexer)
	mi.mu.Unlock()

	for _, idx := range indexers {
		idx.Close()
	}
	mi.lifecycleComplete = true
	return nil
}
