package indexer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

type nestedResolveStateStore struct {
	graph.Store
	mu              sync.Mutex
	generation      int64
	beginCalls      int
	completeCalls   int
	beginErr        error
	completeErr     error
	cancel          context.CancelFunc
	cancelOnReindex bool
	reindexCalls    int
}

func (store *nestedResolveStateStore) BeginResolvePass() (graph.ResolveStateToken, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.beginCalls++
	if store.beginErr != nil {
		return graph.ResolveStateToken{}, store.beginErr
	}
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
	if store.completeErr != nil {
		return store.completeErr
	}
	if token.Generation != store.generation {
		return fmt.Errorf("generation %d is not active", token.Generation)
	}
	store.generation = 0
	return nil
}

func (store *nestedResolveStateStore) ReindexEdges(batch []graph.EdgeReindex) {
	store.Store.ReindexEdges(batch)
	store.mu.Lock()
	store.reindexCalls++
	cancel := store.cancel
	cancelOnReindex := store.cancelOnReindex
	store.mu.Unlock()
	if cancelOnReindex && cancel != nil {
		cancel()
	}
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

func deferredResolveFixture() *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/a.go", Language: "go", RepoPrefix: "repo"})
	g.AddNode(&graph.Node{ID: "repo/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/a.go", Language: "go", RepoPrefix: "repo"})
	g.AddNode(&graph.Node{ID: "repo/a.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "repo/a.go", Language: "go", RepoPrefix: "repo"})
	g.AddEdge(&graph.Edge{From: "repo/a.go::Caller", To: "unresolved::Target", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 1})
	return g
}

func TestResolveDeferredMutationsOwnsWholeFallbackState(t *testing.T) {
	store := &nestedResolveStateStore{Store: graph.New()}
	multi := NewMultiIndexer(store, nil, nil, nil, zap.NewNop())
	receipt := &graph.MutationReceipt{Complete: false, IncompleteReason: "injected"}
	mode, complete, err := multi.resolveDeferredMutationsContext(context.Background(), receipt, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if mode != deferredResolveFallback || !complete {
		t.Fatalf("result = %s/%v, want %s/true", mode, complete, deferredResolveFallback)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.beginCalls < 3 {
		t.Fatalf("begin calls = %d, want outer + nested master + nested cross-repo", store.beginCalls)
	}
	if store.completeCalls != 1 || store.generation != 0 {
		t.Fatalf("fallback state = complete:%d generation:%d, want 1/0", store.completeCalls, store.generation)
	}
}

func TestResolveDeferredMutationsExactCancellationRetainsState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &nestedResolveStateStore{
		Store:           deferredResolveFixture(),
		cancel:          cancel,
		cancelOnReindex: true,
	}
	multi := NewMultiIndexer(store, nil, nil, nil, zap.NewNop())
	receipt := &graph.MutationReceipt{
		Complete:           true,
		ResolutionRelevant: true,
		ChangedFiles:       []string{"repo/a.go"},
	}
	mode, complete, err := multi.resolveDeferredMutationsContext(ctx, receipt, true, nil, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if mode != deferredResolveFailed || complete {
		t.Fatalf("result = %s/%v, want %s/false", mode, complete, deferredResolveFailed)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.reindexCalls == 0 {
		t.Fatal("cancellation fired before the exact master committed a mutation")
	}
	if store.completeCalls != 0 || store.generation == 0 {
		t.Fatalf("cancelled state = complete:%d generation:%d, want 0/non-zero", store.completeCalls, store.generation)
	}
}

func TestResolveDeferredMutationsPropagatesMarkerErrors(t *testing.T) {
	markerErr := errors.New("injected marker failure")
	for _, tc := range []struct {
		name     string
		beginErr error
		endErr   error
	}{
		{name: "begin", beginErr: markerErr},
		{name: "complete", endErr: markerErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &nestedResolveStateStore{Store: graph.New(), beginErr: tc.beginErr, completeErr: tc.endErr}
			multi := NewMultiIndexer(store, nil, nil, nil, zap.NewNop())
			receipt := &graph.MutationReceipt{Complete: true, ChangedFiles: []string{"repo/a.go"}}
			mode, complete, err := multi.resolveDeferredMutationsContext(context.Background(), receipt, true, nil, false)
			if !errors.Is(err, markerErr) {
				t.Fatalf("error = %v, want %v", err, markerErr)
			}
			if mode != deferredResolveFailed || complete {
				t.Fatalf("result = %s/%v, want %s/false", mode, complete, deferredResolveFailed)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			if tc.beginErr != nil && store.completeCalls != 0 {
				t.Fatalf("complete calls after begin failure = %d, want 0", store.completeCalls)
			}
			if tc.endErr != nil && store.generation == 0 {
				t.Fatal("completion failure cleared the active generation")
			}
		})
	}
}

func TestResolveDeferredMutationsJoinsExistingOwner(t *testing.T) {
	store := &nestedResolveStateStore{Store: graph.New(), generation: 303}
	multi := NewMultiIndexer(store, nil, nil, nil, zap.NewNop())
	receipt := &graph.MutationReceipt{Complete: true, ChangedFiles: []string{"repo/a.go"}}
	mode, complete, err := multi.resolveDeferredMutationsContext(context.Background(), receipt, true, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if mode != deferredResolveExact || !complete {
		t.Fatalf("result = %s/%v, want %s/true", mode, complete, deferredResolveExact)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.beginCalls != 1 || store.completeCalls != 0 || store.generation != 303 {
		t.Fatalf("joined state = begin:%d complete:%d generation:%d, want 1/0/303",
			store.beginCalls, store.completeCalls, store.generation)
	}
}

func TestResolveDeferredMutationsNoopAvoidsDurableMarker(t *testing.T) {
	store := &nestedResolveStateStore{Store: graph.New()}
	multi := NewMultiIndexer(store, nil, nil, nil, zap.NewNop())
	mode, complete, err := multi.resolveDeferredMutationsContext(
		context.Background(), &graph.MutationReceipt{Complete: true}, false, nil, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if mode != deferredResolveSkipped || !complete {
		t.Fatalf("result = %s/%v, want %s/true", mode, complete, deferredResolveSkipped)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.beginCalls != 0 || store.completeCalls != 0 {
		t.Fatalf("no-op marker calls = begin:%d complete:%d, want 0/0", store.beginCalls, store.completeCalls)
	}
}
