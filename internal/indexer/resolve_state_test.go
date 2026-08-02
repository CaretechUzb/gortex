package indexer

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

type nestedResolveStateStore struct {
	graph.Store
	mu            sync.Mutex
	generation    int64
	beginCalls    int
	completeCalls int
}

func (store *nestedResolveStateStore) BeginResolvePass() (graph.ResolveStateToken, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.beginCalls++
	if store.generation != 0 {
		return graph.ResolveStateToken{Generation: store.generation}, nil
	}
	store.generation = 101
	return graph.ResolveStateToken{Generation: store.generation, Owned: true}, nil
}

func (store *nestedResolveStateStore) ResolvePassIncomplete() (graph.ResolveStateToken, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	return graph.ResolveStateToken{Generation: store.generation}, store.generation != 0, nil
}

func (store *nestedResolveStateStore) CompleteResolvePass(token graph.ResolveStateToken) error {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.completeCalls++
	if token.Generation != store.generation {
		return fmt.Errorf("generation %d is not active", token.Generation)
	}
	store.generation = 0
	return nil
}

func TestRunPreEnrichResolveOwnsNestedMasterAndCrossRepoState(t *testing.T) {
	store := &nestedResolveStateStore{Store: graph.New()}
	multi := NewMultiIndexer(store, nil, nil, nil, zap.NewNop())
	if err := multi.RunPreEnrichResolve(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.beginCalls < 3 {
		t.Fatalf("begin calls = %d, want outer + master + cross-repo", store.beginCalls)
	}
	if store.completeCalls != 1 || store.generation != 0 {
		t.Fatalf("nested state = complete:%d generation:%d, want 1/0",
			store.completeCalls, store.generation)
	}
}
