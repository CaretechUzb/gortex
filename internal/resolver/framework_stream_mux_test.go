package resolver

import (
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type frameworkIdentityLookupTestStore struct {
	graph.Store
	exactCalls int
	requested  []graph.EdgeIdentity
	broadCalls int
}

func (s *frameworkIdentityLookupTestStore) FindEdgesByIdentities(
	identities []graph.EdgeIdentity,
) map[graph.EdgeIdentity]*graph.Edge {
	s.exactCalls++
	s.requested = append([]graph.EdgeIdentity(nil), identities...)
	finder := s.Store.(graph.EdgeIdentityBatchFinder)
	return finder.FindEdgesByIdentities(identities)
}

func (s *frameworkIdentityLookupTestStore) GetOutEdgesByNodeIDs(
	ids []string,
) map[string][]*graph.Edge {
	s.broadCalls++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

type frameworkBroadOnlyTestStore struct {
	graph.Store
	broadCalls int
}

func (s *frameworkBroadOnlyTestStore) GetOutEdgesByNodeIDs(
	ids []string,
) map[string][]*graph.Edge {
	s.broadCalls++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func TestFrameworkPassCandidatesHydrateOnceByExactIdentity(t *testing.T) {
	g := graph.New()
	first := &graph.Edge{From: "caller", To: "first", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1, Meta: map[string]any{"version": "old"}}
	second := &graph.Edge{From: "caller", To: "second", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 2}
	removed := &graph.Edge{From: "caller", To: "removed", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 3}
	ref := &graph.Edge{From: "caller", To: "ref", Kind: graph.EdgeReferences, FilePath: "a.go", Line: 4}
	annotated := &graph.Edge{From: "caller", To: "annotation", Kind: graph.EdgeAnnotated, FilePath: "a.go", Line: 5}
	g.AddBatch(nil, []*graph.Edge{first, second, removed, ref, annotated})

	firstID := graph.EdgeIdentityFor(first)
	secondID := graph.EdgeIdentityFor(second)
	removedID := graph.EdgeIdentityFor(removed)
	refID := graph.EdgeIdentityFor(ref)
	annotatedID := graph.EdgeIdentityFor(annotated)

	// Same logical key, newer payload: hydration must return the live row.
	g.AddEdge(&graph.Edge{From: first.From, To: first.To, Kind: first.Kind, FilePath: first.FilePath, Line: first.Line, Meta: map[string]any{"version": "live"}})
	// An earlier pass removed this identity: it must disappear from the bundle.
	g.RemoveEdge(removed.From, removed.To, removed.Kind)

	store := &frameworkIdentityLookupTestStore{Store: g}
	sc := &frameworkStreamCandidates{perPass: map[string]*frameworkPassCandidateIdentities{
		"pass": {
			calls:     []graph.EdgeIdentity{secondID, firstID, removedID},
			refs:      []graph.EdgeIdentity{refID},
			annotated: []graph.EdgeIdentity{annotatedID},
		},
	}}
	bundle := sc.passStreams(store, "pass")
	if bundle == nil {
		t.Fatal("exact-capable store returned no candidate bundle")
	}
	if store.exactCalls != 1 || store.broadCalls != 0 {
		t.Fatalf("reads = exact:%d broad:%d, want 1/0", store.exactCalls, store.broadCalls)
	}
	wantRequested := []graph.EdgeIdentity{secondID, firstID, removedID, refID, annotatedID}
	if !reflect.DeepEqual(store.requested, wantRequested) {
		t.Fatalf("requested identities = %#v, want %#v", store.requested, wantRequested)
	}
	if len(bundle.calls) != 2 || bundle.calls[0].To != "second" || bundle.calls[1].To != "first" {
		t.Fatalf("call order/live filtering = %#v", bundle.calls)
	}
	if got := bundle.calls[1].Meta["version"]; got != "live" {
		t.Fatalf("hydrated payload version = %#v, want live", got)
	}
	if len(bundle.refs) != 1 || bundle.refs[0].To != "ref" {
		t.Fatalf("refs = %#v", bundle.refs)
	}
	if len(bundle.annotated) != 1 || bundle.annotated[0].To != "annotation" {
		t.Fatalf("annotated = %#v", bundle.annotated)
	}
}

func TestFrameworkPassCandidatesWithoutExactLookupUseLegacyPath(t *testing.T) {
	g := graph.New()
	edge := &graph.Edge{From: "caller", To: "callee", Kind: graph.EdgeCalls}
	g.AddEdge(edge)
	store := &frameworkBroadOnlyTestStore{Store: g}
	sc := &frameworkStreamCandidates{perPass: map[string]*frameworkPassCandidateIdentities{
		"pass": {calls: []graph.EdgeIdentity{graph.EdgeIdentityFor(edge)}},
	}}
	if bundle := sc.passStreams(store, "pass"); bundle != nil {
		t.Fatalf("bundle = %#v, want nil legacy fallback", bundle)
	}
	if store.broadCalls != 0 {
		t.Fatalf("broad adjacency reads = %d, want 0", store.broadCalls)
	}
}
