package resolver

import (
	"iter"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func seedTerminalPersistenceFixture(store graph.Store) {
	store.AddNode(&graph.Node{
		ID: "caller", Name: "caller", Kind: graph.KindFunction,
		FilePath: "caller.go", Language: "go",
	})
	store.AddNode(&graph.Node{
		ID: "found.go::Found", Name: "Found", Kind: graph.KindFunction,
		FilePath: "found.go", Language: "go",
	})
	store.AddEdge(&graph.Edge{
		From: "caller", To: "unresolved::Missing", Kind: graph.EdgeCalls,
		FilePath: "caller.go", Line: 10,
		Meta: map[string]any{"keep": "missing"},
	})
	store.AddEdge(&graph.Edge{
		From: "caller", To: "unresolved::Found", Kind: graph.EdgeCalls,
		FilePath: "caller.go", Line: 20,
		Meta: map[string]any{
			"keep":                              "found",
			graph.EdgeMetaResolveTerminal:       true,
			graph.EdgeMetaResolveTerminalReason: terminalReasonNoDefinition,
		},
	})
}

func terminalPersistenceEdges(t *testing.T, store graph.Store) map[string]*graph.Edge {
	t.Helper()
	got := make(map[string]*graph.Edge)
	for _, edge := range store.GetOutEdges("caller") {
		got[edge.To] = edge
	}
	if len(got) != 2 {
		t.Fatalf("fixture edge count = %d, want 2", len(got))
	}
	return got
}

func assertTerminalPersistenceResult(t *testing.T, store graph.Store) {
	t.Helper()
	edges := terminalPersistenceEdges(t, store)
	missing := edges["unresolved::Missing"]
	if terminal, _ := missing.Meta[graph.EdgeMetaResolveTerminal].(bool); !terminal {
		t.Fatalf("missing edge was not stamped: %#v", missing.Meta)
	}
	if reason, _ := missing.Meta[graph.EdgeMetaResolveTerminalReason].(string); reason != terminalReasonNoDefinition {
		t.Fatalf("missing reason = %q, want %q", reason, terminalReasonNoDefinition)
	}
	if missing.Meta["keep"] != "missing" {
		t.Fatalf("missing edge metadata changed: %#v", missing.Meta)
	}

	found := edges["unresolved::Found"]
	if _, ok := found.Meta[graph.EdgeMetaResolveTerminal]; ok {
		t.Fatalf("found edge retained terminal stamp: %#v", found.Meta)
	}
	if _, ok := found.Meta[graph.EdgeMetaResolveTerminalReason]; ok {
		t.Fatalf("found edge retained terminal reason: %#v", found.Meta)
	}
	if found.Meta["keep"] != "found" {
		t.Fatalf("found edge metadata changed: %#v", found.Meta)
	}
}

func TestReconcileTerminalStampPersistenceGraphSQLiteParity(t *testing.T) {
	for _, tc := range []struct {
		name string
		open func(*testing.T) graph.Store
	}{
		{
			name: "graph",
			open: func(*testing.T) graph.Store { return graph.New() },
		},
		{
			name: "sqlite",
			open: func(t *testing.T) graph.Store {
				store, err := store_sqlite.Open(":memory:")
				if err != nil {
					t.Fatalf("Open(:memory:): %v", err)
				}
				t.Cleanup(func() {
					if err := store.Close(); err != nil {
						t.Errorf("Close(): %v", err)
					}
				})
				return store
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.open(t)
			seedTerminalPersistenceFixture(store)
			resolver := New(store)
			stamped, unstamped := resolver.reconcileTerminalStamps()
			if stamped != 1 || unstamped != 1 {
				t.Fatalf("reconcile counts = (%d, %d), want (1, 1)", stamped, unstamped)
			}
			assertTerminalPersistenceResult(t, store)

			stamped, unstamped = resolver.reconcileTerminalStamps()
			if stamped != 0 || unstamped != 0 {
				t.Fatalf("converged reconcile counts = (%d, %d), want (0, 0)", stamped, unstamped)
			}
		})
	}
}

type terminalFallbackStore struct {
	graph.Store
	calls     int
	persisted []*graph.Edge
}

func (s *terminalFallbackStore) PersistEdgeAttributesBatch(edges []*graph.Edge) {
	s.calls++
	s.persisted = append(s.persisted[:0], edges...)
}

func TestReconcileTerminalStampsPreservesFullAttributeFallback(t *testing.T) {
	store := &terminalFallbackStore{Store: graph.New()}
	seedTerminalPersistenceFixture(store)
	stamped, unstamped := New(store).reconcileTerminalStamps()
	if stamped != 1 || unstamped != 1 {
		t.Fatalf("reconcile counts = (%d, %d), want (1, 1)", stamped, unstamped)
	}
	if store.calls != 1 || len(store.persisted) != 2 {
		t.Fatalf("fallback calls=%d persisted=%d, want 1/2", store.calls, len(store.persisted))
	}
	assertTerminalPersistenceResult(t, store)
}

type terminalCapabilityProbe struct {
	graph.Store
	scanning           bool
	persistedWhileScan bool
	terminalCalls      int
	fallbackCalls      int
	persisted          []*graph.Edge
}

func (s *terminalCapabilityProbe) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	base := s.Store.EdgesWithUnresolvedTarget()
	return func(yield func(*graph.Edge) bool) {
		s.scanning = true
		defer func() { s.scanning = false }()
		for edge := range base {
			if !yield(edge) {
				return
			}
		}
	}
}

func (s *terminalCapabilityProbe) PersistEdgeTerminalStamps(edges []*graph.Edge) {
	s.terminalCalls++
	s.persistedWhileScan = s.persistedWhileScan || s.scanning
	s.persisted = append(s.persisted[:0], edges...)
}

func (s *terminalCapabilityProbe) PersistEdgeAttributesBatch([]*graph.Edge) {
	s.fallbackCalls++
}

func TestReconcileTerminalStampsPrefersNarrowCapabilityAfterCursorCloses(t *testing.T) {
	store := &terminalCapabilityProbe{Store: graph.New()}
	seedTerminalPersistenceFixture(store)
	stamped, unstamped := New(store).reconcileTerminalStamps()
	if stamped != 1 || unstamped != 1 {
		t.Fatalf("reconcile counts = (%d, %d), want (1, 1)", stamped, unstamped)
	}
	if store.terminalCalls != 1 || store.fallbackCalls != 0 || len(store.persisted) != 2 {
		t.Fatalf("terminal calls=%d fallback calls=%d persisted=%d, want 1/0/2",
			store.terminalCalls, store.fallbackCalls, len(store.persisted))
	}
	if store.persistedWhileScan {
		t.Fatal("terminal persistence started before unresolved-edge cursor completed")
	}
	assertTerminalPersistenceResult(t, store)
}
