package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

type exploreImplementationStoreFixture struct {
	owner     *graph.Node
	seed      *graph.Node
	impl      *graph.Node
	member    *graph.Node
	foldOwner *graph.Node
	foldA     *graph.Node
	foldB     *graph.Node
	nodes     []*graph.Node
	edges     []*graph.Edge
}

func newExploreImplementationStoreFixture() exploreImplementationStoreFixture {
	fixture := exploreImplementationStoreFixture{
		owner:     &graph.Node{ID: "implementation-owner", Kind: graph.KindInterface, Name: "Runner", FilePath: "repo/api/runner.go"},
		seed:      &graph.Node{ID: "implementation-seed", Kind: graph.KindMethod, Name: "run", FilePath: "repo/api/method.go"},
		impl:      &graph.Node{ID: "implementation-type", Kind: graph.KindType, Name: "ConcreteRunner", FilePath: "repo/impl/runner.go"},
		member:    &graph.Node{ID: "implementation-member", Kind: graph.KindMethod, Name: "run", FilePath: "repo/impl/runner.go"},
		foldOwner: &graph.Node{ID: "fold-owner", Kind: graph.KindType, Name: "Options", FilePath: "repo/options/options.go"},
		foldA:     &graph.Node{ID: "fold-member-a", Kind: graph.KindMethod, Name: "encode", FilePath: "repo/options/encode.go"},
		foldB:     &graph.Node{ID: "fold-member-b", Kind: graph.KindMethod, Name: "decode", FilePath: "repo/options/decode.go"},
	}
	fixture.nodes = []*graph.Node{
		fixture.owner, fixture.seed, fixture.impl, fixture.member,
		fixture.foldOwner, fixture.foldA, fixture.foldB,
	}
	fixture.edges = []*graph.Edge{
		{From: fixture.seed.ID, To: fixture.owner.ID, Kind: graph.EdgeMemberOf},
		{From: fixture.impl.ID, To: fixture.owner.ID, Kind: graph.EdgeImplements},
		{From: fixture.member.ID, To: fixture.impl.ID, Kind: graph.EdgeMemberOf},
		{From: fixture.foldA.ID, To: fixture.foldOwner.ID, Kind: graph.EdgeMemberOf},
		{From: fixture.foldB.ID, To: fixture.foldOwner.ID, Kind: graph.EdgeMemberOf},
	}
	return fixture
}

func addExploreImplementationFixtureToStore(store graph.Store, fixture exploreImplementationStoreFixture) {
	store.AddBatch(fixture.nodes, fixture.edges)
}

func requireExploreImplementationStoreParity(t *testing.T, store graph.Store) {
	t.Helper()
	fixture := newExploreImplementationStoreFixture()
	addExploreImplementationFixtureToStore(store, fixture)
	server := &Server{engine: query.NewEngine(store)}

	expanded := server.expandImplementationTargets(
		context.Background(), []exploreTarget{{node: fixture.seed, score: 1}}, graph.LocalizationNodeScope{},
	)
	if len(expanded) != 2 || expanded[0].node.ID != fixture.seed.ID || expanded[1].node.ID != fixture.member.ID {
		t.Fatalf("implementation expansion = %#v", expanded)
	}
	if !exploreImplementationAnswerBlocked(
		context.Background(), "find concrete implementation", []exploreTarget{{node: fixture.seed}},
		store, graph.LocalizationNodeScope{},
	) {
		t.Fatal("abstract-only implementation head did not block answer_ready")
	}

	folded := server.foldMemberOwners(context.Background(), []exploreTarget{
		{node: fixture.foldA, score: 1},
		{node: fixture.foldB, score: .9},
	}, graph.LocalizationNodeScope{})
	if len(folded) != 3 || folded[0].node.ID != fixture.foldOwner.ID {
		t.Fatalf("owner fold = %#v", folded)
	}
}

func TestImplementationRecoveryGraphSQLiteParity(t *testing.T) {
	t.Run("graph", func(t *testing.T) {
		requireExploreImplementationStoreParity(t, graph.New())
	})
	t.Run("sqlite", func(t *testing.T) {
		store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "implementation.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := store.Close(); err != nil {
				t.Errorf("close sqlite store: %v", err)
			}
		}()
		requireExploreImplementationStoreParity(t, store)
	})
}
