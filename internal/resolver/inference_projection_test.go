package resolver

import (
	"iter"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type inferenceProjectionRecordingStore struct {
	graph.Store
	memberProjectionCalls int
	parentProjectionCalls int
	edgeKindScans         map[graph.EdgeKind]int
}

func (s *inferenceProjectionRecordingStore) MemberMethodsByType() map[string][]graph.MemberMethodInfo {
	s.memberProjectionCalls++
	return s.Store.(graph.MemberMethodsByType).MemberMethodsByType()
}

func (s *inferenceProjectionRecordingStore) StructuralParentEdges() []graph.StructuralParentEdgeRow {
	s.parentProjectionCalls++
	return s.Store.(graph.StructuralParentEdges).StructuralParentEdges()
}

func (s *inferenceProjectionRecordingStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	if s.edgeKindScans == nil {
		s.edgeKindScans = make(map[graph.EdgeKind]int)
	}
	s.edgeKindScans[kind]++
	return s.Store.EdgesByKind(kind)
}

type inferenceProjectionOpaqueStore struct{ graph.Store }

type structuralInferenceFixture struct {
	graph        *graph.Graph
	childType    *graph.Node
	iface        *graph.Node
	childOld     *graph.Node
	childWinner  *graph.Node
	parentMethod *graph.Node
}

func newStructuralInferenceFixture() structuralInferenceFixture {
	g := graph.New()
	fixture := structuralInferenceFixture{
		graph: g,
		childType: &graph.Node{
			ID: "repo::Child", Kind: graph.KindType, Name: "Child",
			FilePath: "child.go", StartLine: 10, RepoPrefix: "repo",
		},
		iface: &graph.Node{
			ID: "repo::Runner", Kind: graph.KindInterface, Name: "Runner",
			FilePath: "runner.go", StartLine: 2, RepoPrefix: "repo",
			Meta: map[string]any{"methods": []string{"Run"}},
		},
		childOld: &graph.Node{
			ID: "repo::Child.Run.old", Kind: graph.KindMethod, Name: "Run",
			FilePath: "child_old.go", StartLine: 20, RepoPrefix: "repo",
		},
		childWinner: &graph.Node{
			ID: "repo::Child.Run", Kind: graph.KindMethod, Name: "Run",
			FilePath: "child.go", StartLine: 30, RepoPrefix: "repo",
		},
		parentMethod: &graph.Node{
			ID: "repo::Runner.Run", Kind: graph.KindMethod, Name: "Run",
			FilePath: "runner.go", StartLine: 3, RepoPrefix: "repo",
		},
	}
	g.AddBatch(
		[]*graph.Node{
			fixture.childType, fixture.iface, fixture.childOld,
			fixture.childWinner, fixture.parentMethod,
		},
		[]*graph.Edge{
			{From: fixture.childOld.ID, To: fixture.childType.ID, Kind: graph.EdgeMemberOf},
			// The stable edge-order contract makes this same-name method the
			// last winner for override inference.
			{From: fixture.childWinner.ID, To: fixture.childType.ID, Kind: graph.EdgeMemberOf},
			{From: fixture.parentMethod.ID, To: fixture.iface.ID, Kind: graph.EdgeMemberOf},
		},
	)
	return fixture
}

func TestFullInferenceUsesCompactStructuralProjections(t *testing.T) {
	fixture := newStructuralInferenceFixture()
	store := &inferenceProjectionRecordingStore{Store: fixture.graph}
	resolver := New(store)

	if got := resolver.InferImplements(); got != 1 {
		t.Fatalf("InferImplements added %d edges, want 1", got)
	}
	if got := resolver.InferOverrides(); got != 1 {
		t.Fatalf("InferOverrides added %d edges, want 1", got)
	}
	if store.memberProjectionCalls != 2 {
		t.Fatalf("MemberMethodsByType calls = %d, want 2 (one per pass)", store.memberProjectionCalls)
	}
	if store.parentProjectionCalls != 1 {
		t.Fatalf("StructuralParentEdges calls = %d, want 1", store.parentProjectionCalls)
	}
	for _, kind := range []graph.EdgeKind{
		graph.EdgeMemberOf, graph.EdgeExtends, graph.EdgeComposes,
	} {
		if got := store.edgeKindScans[kind]; got != 0 {
			t.Errorf("legacy full edge scans for %s = %d, want 0", kind, got)
		}
	}
	if !hasEdgeKind(fixture.graph, fixture.childType.ID, fixture.iface.ID, graph.EdgeImplements) {
		t.Fatal("missing inferred implements edge")
	}
	if !hasEdgeKind(fixture.graph, fixture.childWinner.ID, fixture.parentMethod.ID, graph.EdgeOverrides) {
		t.Fatal("missing override edge from stable last-name winner")
	}
	if hasEdgeKind(fixture.graph, fixture.childOld.ID, fixture.parentMethod.ID, graph.EdgeOverrides) {
		t.Fatal("earlier duplicate-name method incorrectly won override inference")
	}
	var override *graph.Edge
	for edge := range fixture.graph.EdgesByKind(graph.EdgeOverrides) {
		override = edge
	}
	if override == nil || override.FilePath != fixture.childWinner.FilePath ||
		override.Line != fixture.childWinner.StartLine {
		t.Fatalf("override location = (%q,%d), want (%q,%d)",
			override.FilePath, override.Line,
			fixture.childWinner.FilePath, fixture.childWinner.StartLine)
	}
}

func TestScopedInferenceKeepsRepoBoundedPaths(t *testing.T) {
	fixture := newStructuralInferenceFixture()
	fixture.graph.RemoveEdge(fixture.childOld.ID, fixture.childType.ID, graph.EdgeMemberOf)
	store := &inferenceProjectionRecordingStore{Store: fixture.graph}
	resolver := New(store)
	scope := map[string]bool{fixture.childType.ID: true, fixture.iface.ID: true}

	if got := resolver.InferImplementsScoped(scope, scope); got != 1 {
		t.Fatalf("InferImplementsScoped added %d edges, want 1", got)
	}
	if got := resolver.InferOverridesScoped(scope); got != 1 {
		t.Fatalf("InferOverridesScoped added %d edges, want 1", got)
	}
	if store.memberProjectionCalls != 0 || store.parentProjectionCalls != 0 {
		t.Fatalf("scoped inference used global projections: member=%d parent=%d",
			store.memberProjectionCalls, store.parentProjectionCalls)
	}
}

func TestFullInferencePreservesOpaqueAdapterFallback(t *testing.T) {
	fixture := newStructuralInferenceFixture()
	// Keep this fallback assertion single-winner: opaque adapter iteration is
	// allowed to differ from the native projection's stable duplicate ordering.
	fixture.graph.RemoveEdge(fixture.childOld.ID, fixture.childType.ID, graph.EdgeMemberOf)
	resolver := New(&inferenceProjectionOpaqueStore{Store: fixture.graph})

	if got := resolver.InferImplements(); got != 1 {
		t.Fatalf("InferImplements fallback added %d edges, want 1", got)
	}
	if got := resolver.InferOverrides(); got != 1 {
		t.Fatalf("InferOverrides fallback added %d edges, want 1", got)
	}
	if !hasEdgeKind(fixture.graph, fixture.childType.ID, fixture.iface.ID, graph.EdgeImplements) {
		t.Fatal("fallback missing inferred implements edge")
	}
	if !hasEdgeKind(fixture.graph, fixture.childWinner.ID, fixture.parentMethod.ID, graph.EdgeOverrides) {
		t.Fatal("fallback missing inferred override edge")
	}
}
