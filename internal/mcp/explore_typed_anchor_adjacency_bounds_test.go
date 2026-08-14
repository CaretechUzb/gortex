package mcp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func exploreTypedAnchorFixtureProjects(fixture exploreTypedAnchorFixture) bool {
	// Boundary cases isolate the intended stage from the fixture's unrelated
	// same-name wrong-type route.
	fixture.reader.in["rs-owner"] = removeExploreTypedAnchorTestEdge(
		fixture.reader.in["rs-owner"], "rs-wrong-consumer", "rs-owner", graph.EdgeMemberOf,
	)
	fixture.reader.out["rs-wrong-consumer"] = removeExploreTypedAnchorTestEdge(
		fixture.reader.out["rs-wrong-consumer"], "rs-wrong-consumer", "rs-owner", graph.EdgeMemberOf,
	)
	got := projectExploreTypedAnchorCandidates(
		context.Background(), fixture.task, fixture.candidates, fixture.reader,
		exploreTypedAnchorTestScope(), len(fixture.candidates), fixture.protected, "",
	)
	return exploreTypedAnchorCandidateByID(got, fixture.member.ID) != nil
}

func addExploreTypedAnchorIrrelevantEdges(
	fixture *exploreTypedAnchorFixture,
	from string,
	toPrefix string,
	kind graph.EdgeKind,
) {
	for index := 0; index < 300; index++ {
		fixture.reader.addEdge(from, fmt.Sprintf("%s-%03d", toPrefix, index), kind)
	}
}

func addExploreTypedAnchorIrrelevantIncomingEdges(
	fixture *exploreTypedAnchorFixture,
	fromPrefix string,
	to string,
	kind graph.EdgeKind,
) {
	for index := 0; index < 300; index++ {
		fixture.reader.addEdge(fmt.Sprintf("%s-%03d", fromPrefix, index), to, kind)
	}
}

func TestProjectExploreTypedAnchorCandidatesExactStageLimits(t *testing.T) {
	t.Run("field relations 8 and 9", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			total int
			want  bool
		}{
			{name: "exact", total: exploreTypedAnchorFieldRelationLimit, want: true},
			{name: "sentinel", total: exploreTypedAnchorFieldRelationLimit + 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newExploreTypedAnchorFixture(
					"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", true,
				)
				for index := 2; index < test.total; index++ {
					node := exploreTypedAnchorNode(fmt.Sprintf("field-type-%02d", index), fmt.Sprintf("Other%d", index), "types.rs", "rs", graph.KindType)
					fixture.reader.nodes[node.ID] = node
					fixture.reader.addEdge(fixture.field.ID, node.ID, graph.EdgeTypedAs)
				}
				addExploreTypedAnchorIrrelevantEdges(&fixture, fixture.field.ID, "field-noise", graph.EdgeCalls)
				if got := exploreTypedAnchorFixtureProjects(fixture); got != test.want {
					t.Fatalf("projection = %v, want %v at %d field relations", got, test.want, test.total)
				}
			})
		}
	})

	t.Run("direct consumers 12 and 13", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			total int
			want  bool
		}{
			{name: "exact", total: exploreTypedAnchorConsumerLimit, want: true},
			{name: "sentinel", total: exploreTypedAnchorConsumerLimit + 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newExploreTypedAnchorFixture(
					"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
				)
				for index := 1; index < test.total; index++ {
					node := exploreTypedAnchorNode(fmt.Sprintf("direct-consumer-%02d", index), "noise", "sink.rs", "rs", graph.KindMethod)
					fixture.reader.nodes[node.ID] = node
					fixture.reader.addEdge(node.ID, "rs-owner", graph.EdgeMemberOf)
					fixture.reader.addEdge(node.ID, fixture.field.ID, graph.EdgeReads)
				}
				addExploreTypedAnchorIrrelevantIncomingEdges(&fixture, "irrelevant-reader", fixture.field.ID, graph.EdgeCalls)
				if got := exploreTypedAnchorFixtureProjects(fixture); got != test.want {
					t.Fatalf("projection = %v, want %v at %d direct consumers", got, test.want, test.total)
				}
			})
		}
	})

	t.Run("owner members 48 and 49", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			total int
			want  bool
		}{
			{name: "exact", total: exploreTypedAnchorOwnerMemberLimit, want: true},
			{name: "sentinel", total: exploreTypedAnchorOwnerMemberLimit + 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newExploreTypedAnchorFixture(
					"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
				)
				for index := 2; index < test.total; index++ {
					node := exploreTypedAnchorNode(fmt.Sprintf("owner-member-%02d", index), "noise", "sink.rs", "rs", graph.KindMethod)
					fixture.reader.nodes[node.ID] = node
					fixture.reader.addEdge(node.ID, "rs-owner", graph.EdgeMemberOf)
				}
				addExploreTypedAnchorIrrelevantIncomingEdges(&fixture, "irrelevant-owner-reader", "rs-owner", graph.EdgeReads)
				if got := exploreTypedAnchorFixtureProjects(fixture); got != test.want {
					t.Fatalf("projection = %v, want %v at %d owner members", got, test.want, test.total)
				}
			})
		}
	})

	t.Run("consumer calls 64 and 65", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			total int
			want  bool
		}{
			{name: "exact", total: exploreTypedAnchorCallLimit, want: true},
			{name: "sentinel", total: exploreTypedAnchorCallLimit + 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newExploreTypedAnchorFixture(
					"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
				)
				for index := 1; index < test.total; index++ {
					node := exploreTypedAnchorNode(fmt.Sprintf("called-member-%02d", index), "noise", "codec.rs", "rs", graph.KindMethod)
					fixture.reader.nodes[node.ID] = node
					fixture.reader.addEdge(fixture.consumer.ID, node.ID, graph.EdgeCalls)
					fixture.reader.addEdge(node.ID, "rs-target-type", graph.EdgeMemberOf)
				}
				addExploreTypedAnchorIrrelevantEdges(&fixture, fixture.consumer.ID, "call-noise", graph.EdgeReads)
				if got := exploreTypedAnchorFixtureProjects(fixture); got != test.want {
					t.Fatalf("projection = %v, want %v at %d calls", got, test.want, test.total)
				}
			})
		}
	})

	t.Run("member owners 4 and 5", func(t *testing.T) {
		for _, test := range []struct {
			name  string
			total int
			want  bool
		}{
			{name: "exact", total: exploreTypedAnchorMemberOwnerLimit, want: true},
			{name: "sentinel", total: exploreTypedAnchorMemberOwnerLimit + 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				fixture := newExploreTypedAnchorFixture(
					"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
				)
				for index := 1; index < test.total; index++ {
					node := exploreTypedAnchorNode(fmt.Sprintf("member-owner-%02d", index), fmt.Sprintf("Other%d", index), "types.rs", "rs", graph.KindType)
					fixture.reader.nodes[node.ID] = node
					fixture.reader.addEdge(fixture.member.ID, node.ID, graph.EdgeMemberOf)
				}
				addExploreTypedAnchorIrrelevantEdges(&fixture, fixture.member.ID, "member-noise", graph.EdgeCalls)
				if got := exploreTypedAnchorFixtureProjects(fixture); got != test.want {
					t.Fatalf("projection = %v, want %v at %d member owners", got, test.want, test.total)
				}
			})
		}
	})
}

func TestProjectExploreTypedAnchorCandidatesAggregateCallLimit(t *testing.T) {
	for _, test := range []struct {
		name     string
		altCalls int
		want     bool
	}{
		{name: "exact 64", altCalls: 32, want: true},
		{name: "sentinel 65", altCalls: 33},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExploreTypedAnchorFixture(
				"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
			)
			alt := exploreTypedAnchorNode("aggregate-consumer", "noise", "sink.rs", "rs", graph.KindMethod)
			fixture.reader.nodes[alt.ID] = alt
			fixture.reader.addEdge(alt.ID, "rs-owner", graph.EdgeMemberOf)
			fixture.reader.addEdge(alt.ID, fixture.field.ID, graph.EdgeReads)

			addCalls := func(from, prefix string, count int) {
				for index := 0; index < count; index++ {
					node := exploreTypedAnchorNode(fmt.Sprintf("%s-%02d", prefix, index), "noise", "codec.rs", "rs", graph.KindMethod)
					fixture.reader.nodes[node.ID] = node
					fixture.reader.addEdge(from, node.ID, graph.EdgeCalls)
					fixture.reader.addEdge(node.ID, "rs-target-type", graph.EdgeMemberOf)
				}
			}
			addCalls(fixture.consumer.ID, "aggregate-primary", 31)
			addCalls(alt.ID, "aggregate-alt", test.altCalls)
			if got := exploreTypedAnchorFixtureProjects(fixture); got != test.want {
				t.Fatalf("projection = %v, want %v with %d aggregate calls", got, test.want, 32+test.altCalls)
			}
		})
	}
}

type exploreTypedAnchorUnsupportedAdjacencyReader struct {
	delegate    *exploreTypedAnchorTestReader
	legacyCalls int
}

func (r *exploreTypedAnchorUnsupportedAdjacencyReader) GetNodesByIDs(ids []string) map[string]*graph.Node {
	return r.delegate.GetNodesByIDs(ids)
}

func (r *exploreTypedAnchorUnsupportedAdjacencyReader) GetInEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	r.legacyCalls++
	panic("typed-anchor projection fell back to legacy incoming adjacency")
}

func (r *exploreTypedAnchorUnsupportedAdjacencyReader) GetOutEdgesByNodeIDs([]string) map[string][]*graph.Edge {
	r.legacyCalls++
	panic("typed-anchor projection fell back to legacy outgoing adjacency")
}

func requireExploreTypedAnchorOriginalCandidates(
	t *testing.T,
	ctx context.Context,
	fixture exploreTypedAnchorFixture,
	reader exploreTypedAnchorBatchReader,
) {
	t.Helper()
	got := projectExploreTypedAnchorCandidates(
		ctx, fixture.task, fixture.candidates, reader,
		exploreTypedAnchorTestScope(), len(fixture.candidates), fixture.protected, "",
	)
	if len(got) != len(fixture.candidates) {
		t.Fatalf("candidate count = %d, want original %d", len(got), len(fixture.candidates))
	}
	for index := range fixture.candidates {
		if got[index] != fixture.candidates[index] {
			t.Fatalf("candidate[%d] changed on incomplete projection: got %#v want %#v", index, got[index], fixture.candidates[index])
		}
	}
	if exploreTypedAnchorCandidateByID(got, fixture.consumer.ID) != nil ||
		exploreTypedAnchorCandidateByID(got, fixture.member.ID) != nil {
		t.Fatal("incomplete projection retained a partial consumer/member pair")
	}
}

func TestProjectExploreTypedAnchorCandidatesFailsClosedOnAdjacencyFaults(t *testing.T) {
	t.Run("unsupported capability never falls back", func(t *testing.T) {
		fixture := newExploreTypedAnchorFixture(
			"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
		)
		reader := &exploreTypedAnchorUnsupportedAdjacencyReader{delegate: fixture.reader}
		requireExploreTypedAnchorOriginalCandidates(t, context.Background(), fixture, reader)
		if reader.legacyCalls != 0 {
			t.Fatalf("legacy adjacency calls = %d, want 0", reader.legacyCalls)
		}
	})

	stages := []struct {
		name      string
		direction string
		ordinal   int
	}{
		{name: "field outgoing", direction: "out", ordinal: 1},
		{name: "field direct incoming", direction: "in", ordinal: 1},
		{name: "owner incoming", direction: "in", ordinal: 2},
		{name: "calls outgoing", direction: "out", ordinal: 2},
		{name: "member outgoing", direction: "out", ordinal: 3},
	}
	faults := []struct {
		name   string
		sticky bool
	}{
		{name: "partial error"},
		{name: "sticky cancellation", sticky: true},
	}
	for _, stage := range stages {
		stage := stage
		t.Run(stage.name, func(t *testing.T) {
			for _, fault := range faults {
				fault := fault
				t.Run(fault.name, func(t *testing.T) {
					fixture := newExploreTypedAnchorFixture(
						"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
					)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					incomingCalls, outgoingCalls := 0, 0
					injected := false
					inject := func(projection graph.BoundedEdgeIdentityProjection) (graph.BoundedEdgeIdentityProjection, error) {
						injected = true
						if fault.sticky {
							cancel()
							return projection, nil
						}
						return projection, errors.New("injected adjacency failure")
					}
					fixture.reader.incomingProjection = func(
						callCtx context.Context,
						ids []string,
						kinds []graph.EdgeKind,
						limit int,
					) (graph.BoundedEdgeIdentityProjection, error) {
						incomingCalls++
						projection, err := exploreTypedAnchorTestEdgeProjection(callCtx, ids, kinds, limit, fixture.reader.in)
						if err != nil || stage.direction != "in" || incomingCalls != stage.ordinal {
							return projection, err
						}
						return inject(projection)
					}
					fixture.reader.outgoingProjection = func(
						callCtx context.Context,
						ids []string,
						kinds []graph.EdgeKind,
						limit int,
					) (graph.BoundedEdgeIdentityProjection, error) {
						outgoingCalls++
						projection, err := exploreTypedAnchorTestEdgeProjection(callCtx, ids, kinds, limit, fixture.reader.out)
						if err != nil || stage.direction != "out" || outgoingCalls != stage.ordinal {
							return projection, err
						}
						return inject(projection)
					}

					requireExploreTypedAnchorOriginalCandidates(t, ctx, fixture, fixture.reader)
					if !injected {
						t.Fatal("fault stage was not reached")
					}
					if fault.sticky && !errors.Is(ctx.Err(), context.Canceled) {
						t.Fatalf("request context error = %v, want context.Canceled", ctx.Err())
					}
				})
			}
		})
	}
}

func TestProjectExploreTypedAnchorCandidatesRejectsInvalidAdjacencyShapes(t *testing.T) {
	stages := []struct {
		name      string
		direction string
		ordinal   int
	}{
		{name: "field outgoing", direction: "out", ordinal: 1},
		{name: "field direct incoming", direction: "in", ordinal: 1},
		{name: "owner incoming", direction: "in", ordinal: 2},
		{name: "calls outgoing", direction: "out", ordinal: 2},
		{name: "member outgoing", direction: "out", ordinal: 3},
	}
	shapes := []struct {
		name  string
		empty bool
	}{
		{name: "empty endpoint", empty: true},
		{name: "mis-keyed endpoint"},
	}
	for _, stage := range stages {
		stage := stage
		t.Run(stage.name, func(t *testing.T) {
			for _, shape := range shapes {
				shape := shape
				t.Run(shape.name, func(t *testing.T) {
					fixture := newExploreTypedAnchorFixture(
						"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
					)
					incomingCalls, outgoingCalls := 0, 0
					injected := false
					mutate := func(
						ids []string,
						projection graph.BoundedEdgeIdentityProjection,
					) graph.BoundedEdgeIdentityProjection {
						identities := append([]graph.EdgeIdentity(nil), projection.ByEndpoint[ids[0]]...)
						if len(identities) == 0 {
							t.Fatal("fixture produced no identity at injected stage")
						}
						if stage.direction == "out" {
							if shape.empty {
								identities[0].From = ""
							} else {
								identities[0].From = "mis-keyed-source"
							}
						} else if shape.empty {
							identities[0].To = ""
						} else {
							identities[0].To = "mis-keyed-target"
						}
						projection.ByEndpoint[ids[0]] = identities
						injected = true
						return projection
					}
					fixture.reader.incomingProjection = func(
						callCtx context.Context,
						ids []string,
						kinds []graph.EdgeKind,
						limit int,
					) (graph.BoundedEdgeIdentityProjection, error) {
						incomingCalls++
						projection, err := exploreTypedAnchorTestEdgeProjection(callCtx, ids, kinds, limit, fixture.reader.in)
						if err == nil && stage.direction == "in" && incomingCalls == stage.ordinal {
							projection = mutate(ids, projection)
						}
						return projection, err
					}
					fixture.reader.outgoingProjection = func(
						callCtx context.Context,
						ids []string,
						kinds []graph.EdgeKind,
						limit int,
					) (graph.BoundedEdgeIdentityProjection, error) {
						outgoingCalls++
						projection, err := exploreTypedAnchorTestEdgeProjection(callCtx, ids, kinds, limit, fixture.reader.out)
						if err == nil && stage.direction == "out" && outgoingCalls == stage.ordinal {
							projection = mutate(ids, projection)
						}
						return projection, err
					}
					requireExploreTypedAnchorOriginalCandidates(t, context.Background(), fixture, fixture.reader)
					if !injected {
						t.Fatal("invalid-shape stage was not reached")
					}
				})
			}
		})
	}

	t.Run("limit plus one without truncation", func(t *testing.T) {
		fixture := newExploreTypedAnchorFixture(
			"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
		)
		injected := false
		fixture.reader.outgoingProjection = func(
			_ context.Context,
			ids []string,
			kinds []graph.EdgeKind,
			limit int,
		) (graph.BoundedEdgeIdentityProjection, error) {
			identities := make([]graph.EdgeIdentity, limit+1)
			for index := range identities {
				identities[index] = graph.EdgeIdentity{
					From: ids[0], To: fmt.Sprintf("dishonest-target-%02d", index), Kind: kinds[0],
				}
			}
			injected = true
			return graph.BoundedEdgeIdentityProjection{
				ByEndpoint: map[string][]graph.EdgeIdentity{ids[0]: identities},
				Truncated:  map[string]bool{},
			}, nil
		}
		requireExploreTypedAnchorOriginalCandidates(t, context.Background(), fixture, fixture.reader)
		if !injected {
			t.Fatal("dishonest capability was not invoked")
		}
	})
}

func TestProjectExploreTypedAnchorCandidatesFailsClosedOnMissingHydration(t *testing.T) {
	for _, test := range []struct {
		name        string
		missingID   string
		wantBatches int
	}{
		{name: "field endpoint", missingID: "rs-owner", wantBatches: 1},
		{name: "consumer", missingID: "rs-consumer", wantBatches: 2},
		{name: "member", missingID: "rs-member", wantBatches: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExploreTypedAnchorFixture(
				"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
			)
			delete(fixture.reader.nodes, test.missingID)
			requireExploreTypedAnchorOriginalCandidates(t, context.Background(), fixture, fixture.reader)
			if fixture.reader.nodeBatches != test.wantBatches {
				t.Fatalf("node batches = %d, want failure at batch %d", fixture.reader.nodeBatches, test.wantBatches)
			}
		})
	}
}

type exploreTypedAnchorFinalGateContext struct {
	context.Context
	armed       bool
	checks      int
	cancelAfter int
}

func (ctx *exploreTypedAnchorFinalGateContext) Err() error {
	if err := ctx.Context.Err(); err != nil {
		return err
	}
	if !ctx.armed {
		return nil
	}
	ctx.checks++
	if ctx.checks >= ctx.cancelAfter {
		return context.Canceled
	}
	return nil
}

func TestProjectExploreTypedAnchorCandidatesRechecksCancellationBeforeReserve(t *testing.T) {
	fixture := newExploreTypedAnchorFixture(
		"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
	)
	gateCtx := &exploreTypedAnchorFinalGateContext{
		Context:     context.Background(),
		cancelAfter: 3,
	}
	fixture.reader.nodeProjection = func(_ context.Context, ids []string) (map[string]*graph.Node, error) {
		nodes := make(map[string]*graph.Node, len(ids))
		for _, id := range ids {
			if node := fixture.reader.nodes[id]; node != nil {
				nodes[id] = node
			}
		}
		if fixture.reader.nodeBatches == 3 {
			gateCtx.armed = true
		}
		return nodes, nil
	}

	requireExploreTypedAnchorOriginalCandidates(t, gateCtx, fixture, fixture.reader)
	if fixture.reader.nodeBatches != 3 {
		t.Fatalf("node batches = %d, want final hydration batch 3", fixture.reader.nodeBatches)
	}
	if err := gateCtx.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("final gate context error = %v, want context.Canceled", err)
	}
}

func TestProjectExploreTypedAnchorCandidatesFailsClosedOnNodeRefetchFaults(t *testing.T) {
	for _, batch := range []int{1, 2, 3} {
		batch := batch
		t.Run(fmt.Sprintf("batch %d", batch), func(t *testing.T) {
			for _, fault := range []struct {
				name   string
				sticky bool
			}{
				{name: "partial error"},
				{name: "sticky cancellation", sticky: true},
			} {
				fault := fault
				t.Run(fault.name, func(t *testing.T) {
					fixture := newExploreTypedAnchorFixture(
						"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
					)
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					injected := false
					fixture.reader.nodeProjection = func(_ context.Context, ids []string) (map[string]*graph.Node, error) {
						nodes := make(map[string]*graph.Node, len(ids))
						for _, id := range ids {
							if node := fixture.reader.nodes[id]; node != nil {
								nodes[id] = node
							}
						}
						if fixture.reader.nodeBatches != batch {
							return nodes, nil
						}
						injected = true
						if fault.sticky {
							cancel()
							return nodes, nil
						}
						return nodes, errors.New("injected node refetch failure")
					}

					requireExploreTypedAnchorOriginalCandidates(t, ctx, fixture, fixture.reader)
					if !injected {
						t.Fatalf("node fault batch %d was not reached", batch)
					}
					if fault.sticky && !errors.Is(ctx.Err(), context.Canceled) {
						t.Fatalf("request context error = %v, want context.Canceled", ctx.Err())
					}
				})
			}
		})
	}
}

func TestProjectExploreTypedAnchorCandidatesFiltersScopeAndTests(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(exploreTypedAnchorFixture)
	}{
		{
			name: "test consumer",
			mutate: func(fixture exploreTypedAnchorFixture) {
				fixture.consumer.Meta = map[string]any{"is_test": true}
			},
		},
		{
			name: "test member",
			mutate: func(fixture exploreTypedAnchorFixture) {
				fixture.member.Meta = map[string]any{"is_test": true}
			},
		},
		{
			name: "owner outside repository scope",
			mutate: func(fixture exploreTypedAnchorFixture) {
				fixture.reader.nodes["rs-owner"].RepoPrefix = "other"
			},
		},
		{
			name: "consumer outside workspace scope",
			mutate: func(fixture exploreTypedAnchorFixture) {
				fixture.consumer.WorkspaceID = "other"
			},
		},
		{
			name: "member outside project scope",
			mutate: func(fixture exploreTypedAnchorFixture) {
				fixture.member.ProjectID = "other"
			},
		},
		{
			name: "declared type outside repository scope",
			mutate: func(fixture exploreTypedAnchorFixture) {
				fixture.reader.nodes["rs-target-type"].RepoPrefix = "other"
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExploreTypedAnchorFixture(
				"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
			)
			test.mutate(fixture)
			requireExploreTypedAnchorOriginalCandidates(t, context.Background(), fixture, fixture.reader)
		})
	}

	t.Run("out-of-scope peer owner is ignored", func(t *testing.T) {
		fixture := newExploreTypedAnchorFixture(
			"rs", "--replace causes duplicate output", "replacer", "Replacer<M>", "replace", "replace_all", false,
		)
		peer := exploreTypedAnchorNode("rs-peer-owner", "PeerSink", "peer.rs", "rs", graph.KindType)
		peer.RepoPrefix = "other"
		fixture.reader.nodes[peer.ID] = peer
		fixture.reader.addEdge(fixture.field.ID, peer.ID, graph.EdgeMemberOf)
		if !exploreTypedAnchorFixtureProjects(fixture) {
			t.Fatal("out-of-scope peer owner made the in-scope proof ambiguous")
		}
	})
}
