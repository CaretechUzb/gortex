package mcp

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
)

func addExploreTypedAnchorFixtureToStore(store graph.Store, fixture exploreTypedAnchorFixture) {
	nodes := make([]*graph.Node, 0, len(fixture.reader.nodes))
	for _, node := range fixture.reader.nodes {
		nodes = append(nodes, node)
	}
	edges := make([]*graph.Edge, 0)
	for _, outgoing := range fixture.reader.out {
		edges = append(edges, outgoing...)
	}
	store.AddBatch(nodes, edges)
}

func exploreTypedAnchorReaderProjects(
	ctx context.Context,
	fixture exploreTypedAnchorFixture,
	reader exploreTypedAnchorBatchReader,
) bool {
	got := projectExploreTypedAnchorCandidates(
		ctx, fixture.task, fixture.candidates, reader,
		exploreTypedAnchorTestScope(), len(fixture.candidates), fixture.protected, "",
	)
	return exploreTypedAnchorCandidateByID(got, fixture.member.ID) != nil
}

func requireExploreTypedAnchorStoreParity(t *testing.T, store graph.Store) {
	t.Helper()
	fixture := newExploreTypedAnchorFixture(
		"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
	)
	addExploreTypedAnchorFixtureToStore(store, fixture)
	if !exploreTypedAnchorReaderProjects(context.Background(), fixture, store) {
		t.Fatal("bounded store reader did not produce the complete typed-anchor proof")
	}

	t.Run("target member tombstone and replacement", func(t *testing.T) {
		tombstone := graph.NewOverlayLayer()
		tombstone.MarkRemoved(fixture.member.Name, fixture.member.ID)
		if exploreTypedAnchorReaderProjects(
			context.Background(), fixture, graph.NewOverlaidView(store, tombstone),
		) {
			t.Fatal("overlay-tombstoned typed member retained a stale base proof")
		}

		targetType := fixture.reader.nodes["rs-target-type"]
		replacement := graph.NewOverlayLayer()
		replacement.MarkFile(fixture.member.FilePath, false)
		replacement.AddNode(fixture.member.FilePath, fixture.member)
		replacement.AddNode(targetType.FilePath, targetType)
		replacement.AddEdge(&graph.Edge{
			From: fixture.member.ID, To: targetType.ID, Kind: graph.EdgeMemberOf,
			FilePath: fixture.member.FilePath,
		})
		if !exploreTypedAnchorReaderProjects(
			context.Background(), fixture, graph.NewOverlaidView(store, replacement),
		) {
			t.Fatal("same-identity member replacement did not restore the current-view proof")
		}
	})

	t.Run("source consumer tombstone and replacement", func(t *testing.T) {
		tombstone := graph.NewOverlayLayer()
		tombstone.MarkRemoved(fixture.consumer.Name, fixture.consumer.ID)
		if exploreTypedAnchorReaderProjects(
			context.Background(), fixture, graph.NewOverlaidView(store, tombstone),
		) {
			t.Fatal("overlay-tombstoned consumer retained a stale base proof")
		}

		owner := fixture.reader.nodes["rs-owner"]
		replacement := graph.NewOverlayLayer()
		replacement.MarkFile(fixture.consumer.FilePath, false)
		replacement.AddNode(fixture.field.FilePath, fixture.field)
		replacement.AddNode(owner.FilePath, owner)
		replacement.AddNode(fixture.consumer.FilePath, fixture.consumer)
		for _, edge := range []*graph.Edge{
			{From: fixture.field.ID, To: owner.ID, Kind: graph.EdgeMemberOf, FilePath: fixture.field.FilePath},
			{From: fixture.consumer.ID, To: owner.ID, Kind: graph.EdgeMemberOf, FilePath: fixture.consumer.FilePath},
			{From: fixture.consumer.ID, To: fixture.field.ID, Kind: graph.EdgeReads, FilePath: fixture.consumer.FilePath},
			{From: fixture.consumer.ID, To: fixture.member.ID, Kind: graph.EdgeCalls, FilePath: fixture.consumer.FilePath},
		} {
			replacement.AddEdge(edge)
		}
		if !exploreTypedAnchorReaderProjects(
			context.Background(), fixture, graph.NewOverlaidView(store, replacement),
		) {
			t.Fatal("same-identity consumer replacement did not restore the current-view proof")
		}
	})
}

func TestProjectExploreTypedAnchorCandidatesGraphSQLiteOverlayParity(t *testing.T) {
	t.Run("graph", func(t *testing.T) {
		requireExploreTypedAnchorStoreParity(t, graph.New())
	})
	t.Run("sqlite", func(t *testing.T) {
		store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "typed-anchor.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := store.Close(); err != nil {
				t.Errorf("close sqlite store: %v", err)
			}
		}()
		requireExploreTypedAnchorStoreParity(t, store)
	})
}

func TestProjectExploreTypedAnchorCandidatesUsesEngineOverlayReader(t *testing.T) {
	fixture := newExploreTypedAnchorFixture(
		"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
	)
	routeStore := graph.New()
	addExploreTypedAnchorFixtureToStore(routeStore, fixture)
	server := &Server{graph: graph.New(), engine: query.NewEngine(routeStore)}
	baseEngine := server.engineFor(context.Background())
	if baseEngine == nil || !exploreTypedAnchorReaderProjects(
		context.Background(), fixture, baseEngine.Reader(),
	) {
		t.Fatal("non-overlay engine reader lost its independently configured graph")
	}
	if exploreTypedAnchorReaderProjects(context.Background(), fixture, server.readerFor(context.Background())) {
		t.Fatal("empty server graph unexpectedly contains the engine's typed route")
	}

	tombstone := graph.NewOverlayLayer()
	tombstone.MarkFile(fixture.member.FilePath, false)
	tombstone.MarkRemoved(fixture.member.Name, fixture.member.ID)
	tombstoneView := graph.NewOverlaidView(routeStore, tombstone)
	tombstoneCtx := WithOverlayView(context.Background(), tombstoneView)
	tombstoneEngine := server.engineFor(tombstoneCtx)
	if tombstoneEngine == nil || tombstoneEngine.Reader() != tombstoneView {
		t.Fatal("engineFor did not bind the request-local overlay reader")
	}
	if exploreTypedAnchorReaderProjects(tombstoneCtx, fixture, tombstoneEngine.Reader()) {
		t.Fatal("request-local engine reader bypassed the overlay tombstone")
	}

	targetType := fixture.reader.nodes["rs-target-type"]
	replacement := graph.NewOverlayLayer()
	replacement.MarkFile(fixture.member.FilePath, false)
	replacement.AddNode(fixture.member.FilePath, fixture.member)
	replacement.AddNode(targetType.FilePath, targetType)
	replacement.AddEdge(&graph.Edge{
		From: fixture.member.ID, To: targetType.ID, Kind: graph.EdgeMemberOf,
		FilePath: fixture.member.FilePath,
	})
	replacementView := graph.NewOverlaidView(routeStore, replacement)
	replacementCtx := WithOverlayView(context.Background(), replacementView)
	if !exploreTypedAnchorReaderProjects(
		replacementCtx, fixture, server.engineFor(replacementCtx).Reader(),
	) {
		t.Fatal("engine overlay reader did not restore the same-identity replacement")
	}
	if !exploreTypedAnchorReaderProjects(context.Background(), fixture, server.engine.Reader()) {
		t.Fatal("overlay requests mutated the server's base engine reader")
	}
}
