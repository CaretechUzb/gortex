package resolver

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type frameworkExactEdgeBackendFactory struct {
	name string
	open func(*testing.T) graph.Store
}

func frameworkExactEdgeBackends() []frameworkExactEdgeBackendFactory {
	return []frameworkExactEdgeBackendFactory{
		{
			name: "graph",
			open: func(*testing.T) graph.Store {
				return graph.New()
			},
		},
		{
			name: "sqlite",
			open: func(t *testing.T) graph.Store {
				store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
				if err != nil {
					t.Fatalf("open SQLite store: %v", err)
				}
				t.Cleanup(func() {
					if err := store.Close(); err != nil {
						t.Errorf("close SQLite store: %v", err)
					}
				})
				return store
			},
		},
	}
}

func addFrameworkExactEdgeFixture(store graph.Store, edges ...*graph.Edge) {
	nodeIDs := make(map[string]struct{}, len(edges)*2)
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		nodeIDs[edge.From] = struct{}{}
		nodeIDs[edge.To] = struct{}{}
	}
	nodes := make([]*graph.Node, 0, len(nodeIDs))
	for id := range nodeIDs {
		nodes = append(nodes, &graph.Node{ID: id, Name: id, Kind: graph.KindFunction})
	}
	store.AddBatch(nodes, edges)
}

func TestFrameworkExactEdgeAdaptersPreserveFinder(t *testing.T) {
	for _, backend := range frameworkExactEdgeBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t)
			expected := &graph.Edge{
				From: "source", To: "target", Kind: graph.EdgeCalls,
				FilePath: "fixture.go", Line: 17, Confidence: 0.91,
				Origin: "expected", Meta: map[string]any{"marker": "expected"},
			}
			nearby := &graph.Edge{
				From: "source", To: "target", Kind: graph.EdgeCalls,
				FilePath: "fixture.go", Line: 18, Confidence: 0.12,
				Origin: "nearby",
			}
			addFrameworkExactEdgeFixture(store, expected, nearby)

			key := graph.EdgeIdentityFor(expected)
			missing := key
			missing.Line++
			missing.To = "missing"
			adapters := []struct {
				name   string
				finder graph.EdgeIdentityBatchFinder
			}{
				{name: "batch", finder: newFrameworkEdgeBatchStore(store)},
				{name: "scoped", finder: newFrameworkScopedStore(store, nil, nil)},
				{
					name: "scoped_batch",
					finder: newFrameworkEdgeBatchStore(
						newFrameworkScopedStore(store, nil, nil),
					),
				},
			}
			for _, adapter := range adapters {
				t.Run(adapter.name, func(t *testing.T) {
					got := adapter.finder.FindEdgesByIdentities([]graph.EdgeIdentity{key, missing, key})
					if len(got) != 1 {
						t.Fatalf("got %d exact edges, want 1: %#v", len(got), got)
					}
					edge := got[key]
					if edge == nil {
						t.Fatalf("missing requested identity %#v", key)
					}
					if identity := graph.EdgeIdentityFor(edge); identity != key {
						t.Fatalf("returned identity %#v, want %#v", identity, key)
					}
					if edge.Confidence != expected.Confidence || edge.Origin != expected.Origin {
						t.Fatalf("returned payload confidence=%v origin=%q, want %v/%q", edge.Confidence, edge.Origin, expected.Confidence, expected.Origin)
					}
					if marker, _ := edge.Meta["marker"].(string); marker != "expected" {
						t.Fatalf("returned metadata marker %q, want expected", marker)
					}
				})
			}
		})
	}
}

type frameworkStoreWithoutExactFinder struct {
	graph.Store
	batchCalls int
	fromIDs    []string
}

func (s *frameworkStoreWithoutExactFinder) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.batchCalls++
	s.fromIDs = append([]string(nil), ids...)
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func TestFrameworkExactEdgeFallbackFiltersBroadAdjacency(t *testing.T) {
	store := graph.New()
	expected := &graph.Edge{
		From: "source", To: "target", Kind: graph.EdgeCalls,
		FilePath: "fixture.go", Line: 17,
	}
	addFrameworkExactEdgeFixture(store,
		expected,
		&graph.Edge{From: "source", To: "target", Kind: graph.EdgeCalls, FilePath: "fixture.go", Line: 18},
		&graph.Edge{From: "source", To: "other", Kind: graph.EdgeCalls, FilePath: "fixture.go", Line: 17},
		&graph.Edge{From: "source", To: "target", Kind: graph.EdgeReferences, FilePath: "fixture.go", Line: 17},
	)
	hidden := &frameworkStoreWithoutExactFinder{Store: store}
	if _, exposed := any(hidden).(graph.EdgeIdentityBatchFinder); exposed {
		t.Fatal("test wrapper unexpectedly exposes the optional exact finder")
	}

	finder := newFrameworkScopedStore(hidden, nil, nil)
	key := graph.EdgeIdentityFor(expected)
	missing := key
	missing.Line = 99
	got := finder.FindEdgesByIdentities([]graph.EdgeIdentity{key, missing, key})
	if len(got) != 1 || got[key] == nil {
		t.Fatalf("fallback returned %#v, want only %#v", got, key)
	}
	if identity := graph.EdgeIdentityFor(got[key]); identity != key {
		t.Fatalf("fallback returned identity %#v, want %#v", identity, key)
	}
	if hidden.batchCalls != 1 {
		t.Fatalf("broad fallback made %d adjacency calls, want 1", hidden.batchCalls)
	}
	if len(hidden.fromIDs) != 1 || hidden.fromIDs[0] != key.From {
		t.Fatalf("broad fallback requested sources %#v, want [%q]", hidden.fromIDs, key.From)
	}

	hidden.batchCalls = 0
	hidden.fromIDs = nil
	batch := newFrameworkEdgeBatchStore(hidden)
	staged := *expected
	staged.Origin = "staged"
	batch.AddEdge(&staged)
	got = batch.FindEdgesByIdentities([]graph.EdgeIdentity{key, key})
	if edge := got[key]; edge == nil || edge.Origin != "staged" {
		t.Fatalf("staged-only lookup returned %#v, want staged payload", edge)
	}
	if hidden.batchCalls != 0 {
		t.Fatalf("staged-only lookup made %d durable adjacency calls, want 0", hidden.batchCalls)
	}
}

func TestFrameworkEdgeBatchExactFinderUsesLatestStagedPayload(t *testing.T) {
	for _, backend := range frameworkExactEdgeBackends() {
		t.Run(backend.name, func(t *testing.T) {
			store := backend.open(t)
			base := &graph.Edge{
				From: "source", To: "target", Kind: graph.EdgeCalls,
				FilePath: "fixture.go", Line: 17, Confidence: 0.1,
				Origin: "base", Meta: map[string]any{"marker": "base"},
			}
			addFrameworkExactEdgeFixture(store, base)
			batch := newFrameworkEdgeBatchStore(store)
			key := graph.EdgeIdentityFor(base)

			batch.AddEdge(&graph.Edge{
				From: key.From, To: key.To, Kind: key.Kind,
				FilePath: key.FilePath, Line: key.Line, Confidence: 0.4,
				Origin: "first",
			})
			latest := &graph.Edge{
				From: key.From, To: key.To, Kind: key.Kind,
				FilePath: key.FilePath, Line: key.Line, Confidence: 0.8,
				Origin: "latest",
				Meta: map[string]any{
					"nested": map[string]any{"marker": "latest"},
				},
			}
			batch.AddEdge(latest)
			latest.Confidence = 0.99
			latest.Meta["nested"].(map[string]any)["marker"] = "mutated"
			batch.AddEdge(&graph.Edge{
				From: "unrelated", To: "other", Kind: graph.EdgeReferences,
				FilePath: "other.go", Line: 1, Origin: "unrelated",
			})

			missing := key
			missing.Line++
			got := batch.FindEdgesByIdentities([]graph.EdgeIdentity{key, missing, key})
			if len(got) != 1 {
				t.Fatalf("overlay returned %d edges, want 1: %#v", len(got), got)
			}
			edge := got[key]
			if edge == nil || edge.Confidence != 0.8 || edge.Origin != "latest" {
				t.Fatalf("overlay payload = %#v, want latest staged payload", edge)
			}
			nested, _ := edge.Meta["nested"].(map[string]any)
			if marker, _ := nested["marker"].(string); marker != "latest" {
				t.Fatalf("staged metadata marker %q, want immutable clone latest", marker)
			}

			persisted := &graph.Edge{
				From: key.From, To: key.To, Kind: key.Kind,
				FilePath: key.FilePath, Line: key.Line, Confidence: 0.95,
				Origin: "persisted",
			}
			batch.AddBatch(nil, []*graph.Edge{persisted})
			durable := findFrameworkEdgesByIdentities(store, []graph.EdgeIdentity{key})[key]
			got = batch.FindEdgesByIdentities([]graph.EdgeIdentity{key})
			edge = got[key]
			if durable == nil || edge == nil {
				t.Fatalf("post-AddBatch payload = %#v, durable = %#v", edge, durable)
			}
			if edge.Confidence != durable.Confidence || edge.Origin != durable.Origin {
				t.Fatalf("post-AddBatch payload = %#v, want durable backend payload %#v", edge, durable)
			}
			if edge.Origin == "latest" {
				t.Fatalf("post-AddBatch lookup still returned cleared staged payload: %#v", edge)
			}
		})
	}
}
