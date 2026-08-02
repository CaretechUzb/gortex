package resolver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

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

type failingGuardWorkSpool struct {
	appendErr error
	readErr   error
}

func (spool *failingGuardWorkSpool) appendJobs([][]reindexJob) error { return spool.appendErr }
func (spool *failingGuardWorkSpool) nextPage(int) ([]resolveGuardRecord, bool, error) {
	return nil, false, spool.readErr
}
func (*failingGuardWorkSpool) close() {}

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

func durableResolverFixture(language, extension string, names ...string) *graph.Graph {
	g := graph.New()
	callerFile := "repo/caller." + extension
	targetFile := "repo/target." + extension
	g.AddNode(&graph.Node{ID: callerFile, Kind: graph.KindFile, Name: "caller." + extension, FilePath: callerFile, Language: language, RepoPrefix: "repo"})
	g.AddNode(&graph.Node{ID: targetFile, Kind: graph.KindFile, Name: "target." + extension, FilePath: targetFile, Language: language, RepoPrefix: "repo"})
	callerID := callerFile + "::Caller"
	g.AddNode(&graph.Node{ID: callerID, Kind: graph.KindFunction, Name: "Caller", FilePath: callerFile, StartLine: 1, Language: language, RepoPrefix: "repo"})
	for i, name := range names {
		targetID := targetFile + "::" + name
		g.AddNode(&graph.Node{ID: targetID, Kind: graph.KindFunction, Name: name, FilePath: targetFile, StartLine: i + 10, Language: language, RepoPrefix: "repo"})
		g.AddEdge(&graph.Edge{From: callerID, To: "unresolved::" + name, Kind: graph.EdgeCalls, FilePath: callerFile, Line: i + 1})
	}
	return g
}

func assertResolveMarkerRetained(t *testing.T, store *recordingResolveStateStore) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.reindexCalls == 0 {
		t.Fatal("failure fired before any resolver page committed")
	}
	if store.completeCalls != 0 || store.generation == 0 {
		t.Fatalf("failed pass = complete:%d generation:%d, want 0/non-zero",
			store.completeCalls, store.generation)
	}
}

func TestResolveAllContextGuardSpoolFailuresAfterCommittedPageRetainMarker(t *testing.T) {
	spoolErr := errors.New("injected guard spool failure")
	for _, tc := range []struct {
		name      string
		appendErr error
		readErr   error
	}{
		{name: "append", appendErr: spoolErr},
		{name: "read", readErr: spoolErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingResolveStateStore{Store: durableResolverFixture("go", "go", "Target")}
			r := New(store)
			r.guardSpoolFactory = func() (resolveGuardWorkSpool, error) {
				return &failingGuardWorkSpool{appendErr: tc.appendErr, readErr: tc.readErr}, nil
			}

			if _, err := r.ResolveAllContext(context.Background()); !errors.Is(err, spoolErr) {
				t.Fatalf("ResolveAllContext error = %v, want %v", err, spoolErr)
			}
			assertResolveMarkerRetained(t, store)
		})
	}
}

func TestResolveAllContextDeferredLSPFailuresAfterCommittedPageRetainMarker(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*testing.T, *Resolver) func() error
		want  string
		names []string
	}{
		{
			name:  "append",
			want:  "append deferred LSP spool",
			names: []string{"Alpha", "Beta"},
			setup: func(t *testing.T, r *Resolver) func() error {
				t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")
				closed := false
				r.chunkYieldHook = func() {
					if !closed && r.lspDeferredSpool != nil {
						closed = true
						_ = r.lspDeferredSpool.db.Close()
					}
				}
				return func() error { return nil }
			},
		},
		{
			name:  "iterator read",
			want:  "read deferred LSP spool",
			names: []string{"Alpha"},
			setup: func(_ *testing.T, r *Resolver) func() error {
				var hookErr error
				r.OnComputeDone = func() { hookErr = r.lspDeferredSpool.db.Close() }
				return func() error { return hookErr }
			},
		},
		{
			name:  "completed delete",
			want:  "delete completed deferred LSP work",
			names: []string{"Alpha"},
			setup: func(_ *testing.T, r *Resolver) func() error {
				var hookErr error
				r.OnComputeDone = func() {
					_, hookErr = r.lspDeferredSpool.db.Exec(`CREATE TRIGGER fail_delete
BEFORE DELETE ON work BEGIN SELECT RAISE(FAIL, 'injected delete failure'); END`)
				}
				return func() error { return hookErr }
			},
		},
		{
			name:  "drain update",
			want:  "mark drained deferred LSP work",
			names: []string{"Alpha"},
			setup: func(_ *testing.T, r *Resolver) func() error {
				anchor := time.Unix(1, 0)
				clockReads := 0
				r.SetLSPResolvePassBudget(time.Nanosecond)
				r.lspNow = func() time.Time {
					clockReads++
					if clockReads == 1 {
						return anchor
					}
					return anchor.Add(lspPhaseCutoffFloor + time.Second)
				}
				var hookErr error
				r.OnComputeDone = func() {
					_, hookErr = r.lspDeferredSpool.db.Exec(`CREATE TRIGGER fail_update
BEFORE UPDATE ON work BEGIN SELECT RAISE(FAIL, 'injected update failure'); END`)
				}
				return func() error { return hookErr }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &recordingResolveStateStore{Store: durableResolverFixture("typescript", "ts", tc.names...)}
			r := New(store)
			r.SetLSPHelper(&slowLSPBudgetHelper{ok: false})
			r.SetLSPResolvePassBudget(0)
			hookResult := tc.setup(t, r)

			_, err := r.ResolveAllContext(context.Background())
			if hookErr := hookResult(); hookErr != nil {
				t.Fatalf("install failure injection: %v", hookErr)
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ResolveAllContext error = %v, want substring %q", err, tc.want)
			}
			assertResolveMarkerRetained(t, store)
			if r.lspDeferredSpool != nil {
				r.lspDeferredSpool.close()
				r.lspDeferredSpool = nil
			}
		})
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
