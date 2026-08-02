package resolver

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type recordingResolveStateStore struct {
	graph.Store

	mu              sync.Mutex
	generation      int64
	beginCalls      int
	completeCalls   int
	beginErr        error
	completeErr     error
	cancel          context.CancelFunc
	reindexCalls    int
	cancelOnReindex bool
}

func (store *recordingResolveStateStore) BeginResolvePass() (graph.ResolveStateToken, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.beginCalls++
	if store.beginErr != nil {
		return graph.ResolveStateToken{}, store.beginErr
	}
	if store.generation != 0 {
		return graph.ResolveStateToken{Generation: store.generation}, nil
	}
	store.generation = 41
	return graph.ResolveStateToken{Generation: store.generation, Owned: true}, nil
}

func (store *recordingResolveStateStore) ResolvePassIncomplete() (graph.ResolveStateToken, bool, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.generation == 0 {
		return graph.ResolveStateToken{}, false, nil
	}
	return graph.ResolveStateToken{Generation: store.generation}, true, nil
}

func (store *recordingResolveStateStore) CompleteResolvePass(token graph.ResolveStateToken) error {
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

func (store *recordingResolveStateStore) ReindexEdges(batch []graph.EdgeReindex) {
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

func TestResolveAllContextOwnsAndCompletesDurableState(t *testing.T) {
	store := &recordingResolveStateStore{Store: graph.New()}
	if _, err := New(store).ResolveAllContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.beginCalls != 1 || store.completeCalls != 1 || store.generation != 0 {
		t.Fatalf("state calls = begin:%d complete:%d generation:%d, want 1/1/0",
			store.beginCalls, store.completeCalls, store.generation)
	}
}

func TestResolveAllContextPropagatesDurableStateErrors(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		markerErr := errors.New("begin marker failed")
		store := &recordingResolveStateStore{Store: graph.New(), beginErr: markerErr}
		if _, err := New(store).ResolveAllContext(context.Background()); !errors.Is(err, markerErr) {
			t.Fatalf("ResolveAllContext error = %v, want %v", err, markerErr)
		}
		if store.completeCalls != 0 {
			t.Fatalf("complete calls = %d, want 0", store.completeCalls)
		}
	})

	t.Run("complete", func(t *testing.T) {
		markerErr := errors.New("complete marker failed")
		store := &recordingResolveStateStore{Store: graph.New(), completeErr: markerErr}
		if _, err := New(store).ResolveAllContext(context.Background()); !errors.Is(err, markerErr) {
			t.Fatalf("ResolveAllContext error = %v, want %v", err, markerErr)
		}
		if store.generation == 0 {
			t.Fatal("failed completion cleared the active generation")
		}
	})
}

func TestResolveAllContextCancellationAfterCommittedPageRetainsMarker(t *testing.T) {
	base := graph.New()
	base.AddNode(&graph.Node{ID: "repo/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/a.go", Language: "go", RepoPrefix: "repo"})
	base.AddNode(&graph.Node{ID: "repo/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/a.go", Language: "go", RepoPrefix: "repo"})
	base.AddNode(&graph.Node{ID: "repo/a.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "repo/a.go", Language: "go", RepoPrefix: "repo"})
	for line := 1; line <= resolvePendingPageRows+1; line++ {
		base.AddEdge(&graph.Edge{
			From:     "repo/a.go::Caller",
			To:       "unresolved::Target",
			Kind:     graph.EdgeCalls,
			FilePath: "repo/a.go",
			Line:     line,
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	store := &recordingResolveStateStore{
		Store:           base,
		cancel:          cancel,
		cancelOnReindex: true,
	}

	if _, err := New(store).ResolveAllContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolveAllContext error = %v, want context.Canceled", err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.reindexCalls == 0 {
		t.Fatal("cancellation fired before any resolver page committed")
	}
	if store.completeCalls != 0 || store.generation == 0 {
		t.Fatalf("cancelled pass = complete:%d generation:%d, want 0/non-zero",
			store.completeCalls, store.generation)
	}
}

func TestCrossRepoResolveAllContextOwnsAndCompletesDurableState(t *testing.T) {
	store := &recordingResolveStateStore{Store: graph.New()}
	if _, err := NewCrossRepo(store).ResolveAllContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.beginCalls != 1 || store.completeCalls != 1 || store.generation != 0 {
		t.Fatalf("state calls = begin:%d complete:%d generation:%d, want 1/1/0",
			store.beginCalls, store.completeCalls, store.generation)
	}
}
