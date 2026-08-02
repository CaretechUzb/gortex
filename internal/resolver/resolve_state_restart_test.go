package resolver

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type cancelAfterSQLiteReindex struct {
	*store_sqlite.Store
	cancel context.CancelFunc
	once   sync.Once
}

func (store *cancelAfterSQLiteReindex) ReindexEdges(batch []graph.EdgeReindex) {
	store.Store.ReindexEdges(batch)
	if len(batch) > 0 {
		store.once.Do(store.cancel)
	}
}

func TestInterruptedResolveRestartRequiresFullSourceReconstruction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	addWeakCrossPackageFixture(store)

	ctx, cancel := context.WithCancel(context.Background())
	interrupting := &cancelAfterSQLiteReindex{Store: store, cancel: cancel}
	if _, err := New(interrupting).ResolveAllContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted resolve error = %v, want context.Canceled", err)
	}
	if got := soleCallTarget(t, store); got == "unresolved::Target" {
		t.Fatal("test did not interrupt after the weak call rewrite committed")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = store_sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	observed, incomplete, err := store.ResolvePassIncomplete()
	if err != nil {
		t.Fatal(err)
	}
	if !incomplete {
		t.Fatal("interrupted resolve generation did not survive reopen")
	}

	// A plain rerun is insufficient: the already-rewritten call is outside the
	// unresolved pager, so the resolver cannot revisit or guard it.
	if _, err := New(store).ResolveAllContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := soleCallTarget(t, store); got == "unresolved::Target" {
		t.Fatal("plain ResolveAll unexpectedly reconstructed the committed source target")
	}

	// This is the source reconstruction performed by daemon recovery's forced
	// TrackRepoCtx path: evict the repository's derived rows and re-emit the
	// parser's original unresolved edge before the nil-scope master/cross tail.
	store.EvictRepo("repo")
	addWeakCrossPackageFixture(store)
	if _, err := New(store).ResolveAllContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := soleCallTarget(t, store); got != "unresolved::Target" {
		t.Fatalf("full source reconstruction left weak cross-package target %q", got)
	}
	if err := store.CompleteResolvePass(observed); err != nil {
		t.Fatal(err)
	}
	if _, incomplete, err := store.ResolvePassIncomplete(); err != nil || incomplete {
		t.Fatalf("recovery completion = (incomplete=%v, err=%v), want clean", incomplete, err)
	}
}

func addWeakCrossPackageFixture(store graph.Store) {
	for _, node := range []*graph.Node{
		{ID: "repo/pkg/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/pkg/a.go", Language: "go", RepoPrefix: "repo"},
		{ID: "repo/other/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "repo/other/b.go", Language: "go", RepoPrefix: "repo"},
		{ID: "repo/pkg/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo/pkg/a.go", Language: "go", RepoPrefix: "repo"},
		{ID: "repo/other/b.go::Target", Kind: graph.KindFunction, Name: "Target", FilePath: "repo/other/b.go", Language: "go", RepoPrefix: "repo"},
	} {
		store.AddNode(node)
	}
	store.AddEdge(&graph.Edge{
		From:     "repo/pkg/a.go::Caller",
		To:       "unresolved::Target",
		Kind:     graph.EdgeCalls,
		FilePath: "repo/pkg/a.go",
		Line:     7,
	})
}

func soleCallTarget(t *testing.T, store graph.Store) string {
	t.Helper()
	edges := store.GetOutEdges("repo/pkg/a.go::Caller")
	if len(edges) != 1 {
		t.Fatalf("caller edges = %d, want 1", len(edges))
	}
	return edges[0].To
}
