package resolver

import (
	"fmt"
	"iter"
	"reflect"
	"sort"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type csharpIfaceDispatchFixture struct {
	store           *graph.Graph
	callerID        string
	familyMemberIDs []string
}

func newCSharpIfaceDispatchFixture(irrelevantCalls int) csharpIfaceDispatchFixture {
	const (
		repoID       = "csharp-repo"
		ifaceTypeID  = "csharp-repo::IRunner"
		implATypeID  = "csharp-repo::RunnerA"
		implBTypeID  = "csharp-repo::RunnerB"
		ifaceMethod  = "csharp-repo::IRunner.Run"
		implAMethod  = "csharp-repo::RunnerA.Run"
		implBMethod  = "csharp-repo::RunnerB.Run"
		callerID     = "csharp-repo::Caller.Invoke"
		specCallerID = "csharp-repo::SpecCaller.Invoke"
		priorCaller  = "csharp-repo::PriorCaller.Invoke"
		otherMethod  = "csharp-repo::Other.Run"
	)

	g := graph.New()
	for _, n := range []*graph.Node{
		{ID: ifaceTypeID, Kind: graph.KindType, Name: "IRunner", FilePath: "IRunner.cs", Language: "csharp", RepoPrefix: repoID},
		{ID: implATypeID, Kind: graph.KindType, Name: "RunnerA", FilePath: "RunnerA.cs", Language: "csharp", RepoPrefix: repoID},
		{ID: implBTypeID, Kind: graph.KindType, Name: "RunnerB", FilePath: "RunnerB.cs", Language: "csharp", RepoPrefix: repoID},
		{ID: ifaceMethod, Kind: graph.KindMethod, Name: "Run", FilePath: "IRunner.cs", Language: "csharp", RepoPrefix: repoID, Meta: map[string]any{"iface_member": true}},
		{ID: implAMethod, Kind: graph.KindMethod, Name: "Run", FilePath: "RunnerA.cs", Language: "csharp", RepoPrefix: repoID},
		{ID: implBMethod, Kind: graph.KindMethod, Name: "Run", FilePath: "RunnerB.cs", Language: "csharp", RepoPrefix: repoID},
		{ID: callerID, Kind: graph.KindMethod, Name: "Invoke", FilePath: "Caller.cs", Language: "csharp", RepoPrefix: repoID},
		{ID: specCallerID, Kind: graph.KindMethod, Name: "Invoke", FilePath: "SpecCaller.cs", Language: "csharp", RepoPrefix: repoID},
		{ID: priorCaller, Kind: graph.KindMethod, Name: "Invoke", FilePath: "PriorCaller.cs", Language: "csharp", RepoPrefix: repoID},
		{ID: otherMethod, Kind: graph.KindMethod, Name: "Run", FilePath: "Other.cs", Language: "csharp", RepoPrefix: repoID},
	} {
		g.AddNode(n)
	}

	for _, e := range []*graph.Edge{
		{From: implATypeID, To: ifaceTypeID, Kind: graph.EdgeImplements, FilePath: "RunnerA.cs", Line: 3},
		{From: implBTypeID, To: ifaceTypeID, Kind: graph.EdgeImplements, FilePath: "RunnerB.cs", Line: 3},
		{From: ifaceMethod, To: ifaceTypeID, Kind: graph.EdgeMemberOf, FilePath: "IRunner.cs", Line: 5},
		{From: implAMethod, To: implATypeID, Kind: graph.EdgeMemberOf, FilePath: "RunnerA.cs", Line: 8},
		{From: implBMethod, To: implBTypeID, Kind: graph.EdgeMemberOf, FilePath: "RunnerB.cs", Line: 8},
		{From: callerID, To: ifaceMethod, Kind: graph.EdgeCalls, FilePath: "Caller.cs", Line: 17, Origin: graph.OriginASTInferred, Meta: map[string]any{"source_marker": "full-row"}},
		{From: specCallerID, To: ifaceMethod, Kind: graph.EdgeCalls, FilePath: "SpecCaller.cs", Line: 23, Origin: graph.OriginASTInferred, Meta: map[string]any{graph.MetaSpeculative: true}},
		{From: priorCaller, To: ifaceMethod, Kind: graph.EdgeCalls, FilePath: "PriorCaller.cs", Line: 31, Origin: graph.OriginASTInferred, Meta: map[string]any{MetaSynthesizedBy: SynthCSharpIfaceDispatch}},
	} {
		g.AddEdge(e)
	}

	payload := string(make([]byte, 4096))
	for i := 0; i < irrelevantCalls; i++ {
		g.AddEdge(&graph.Edge{
			From: callerID, To: otherMethod, Kind: graph.EdgeCalls,
			FilePath: "Noise.cs", Line: i + 1, Origin: graph.OriginASTInferred,
			Meta: map[string]any{"payload": payload, "ordinal": i},
		})
	}

	return csharpIfaceDispatchFixture{
		store:           g,
		callerID:        callerID,
		familyMemberIDs: []string{ifaceMethod, implAMethod, implBMethod},
	}
}

// csharpIfaceLegacyIncomingStore models the old full EdgeCalls scan, followed
// by the same family-target filter now performed by the store's incoming
// adjacency lookup. It is intentionally test-only reference behavior.
type csharpIfaceLegacyIncomingStore struct {
	graph.Store
}

func (s *csharpIfaceLegacyIncomingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	wanted := make(map[string]struct{}, len(ids))
	out := make(map[string][]*graph.Edge, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		wanted[id] = struct{}{}
		out[id] = nil
	}
	for edge := range s.EdgesByKind(graph.EdgeCalls) {
		if edge == nil {
			continue
		}
		if _, ok := wanted[edge.To]; ok {
			out[edge.To] = append(out[edge.To], edge)
		}
	}
	return out
}

type csharpIfaceIncomingSpyStore struct {
	graph.Store
	callScans       int
	incomingLookups int
	incomingNodeIDs []string
}

func (s *csharpIfaceIncomingSpyStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	if kind == graph.EdgeCalls {
		s.callScans++
	}
	return s.Store.EdgesByKind(kind)
}

func (s *csharpIfaceIncomingSpyStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.incomingLookups++
	s.incomingNodeIDs = append(s.incomingNodeIDs, ids...)
	return s.Store.GetInEdgesByNodeIDs(ids)
}

type csharpIfaceDispatchSnapshot struct {
	from, to, file, origin, label string
	line                          int
	confidence                    float64
	via, iface, candidates        string
	synthesizedBy, provenance     string
}

func csharpIfaceDispatchSnapshots(g graph.Store) []csharpIfaceDispatchSnapshot {
	var out []csharpIfaceDispatchSnapshot
	for edge := range g.EdgesByKind(graph.EdgeCalls) {
		if edge == nil || edge.Meta == nil || edge.Meta[MetaSynthesizedBy] != SynthCSharpIfaceDispatch {
			continue
		}
		out = append(out, csharpIfaceDispatchSnapshot{
			from: edge.From, to: edge.To, file: edge.FilePath, line: edge.Line,
			origin: edge.Origin, confidence: edge.Confidence, label: edge.ConfidenceLabel,
			via: fmt.Sprint(edge.Meta["via"]), iface: fmt.Sprint(edge.Meta["iface_type"]),
			candidates:    fmt.Sprint(edge.Meta["candidate_count"]),
			synthesizedBy: fmt.Sprint(edge.Meta[MetaSynthesizedBy]),
			provenance:    fmt.Sprint(edge.Meta[MetaProvenance]),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.from != b.from {
			return a.from < b.from
		}
		if a.to != b.to {
			return a.to < b.to
		}
		if a.file != b.file {
			return a.file < b.file
		}
		return a.line < b.line
	})
	return out
}

func TestResolveCSharpInterfaceDispatchTargetedIncomingParity(t *testing.T) {
	targeted := newCSharpIfaceDispatchFixture(37)
	legacy := newCSharpIfaceDispatchFixture(37)

	gotCount := ResolveCSharpInterfaceDispatch(targeted.store)
	wantCount := ResolveCSharpInterfaceDispatch(&csharpIfaceLegacyIncomingStore{Store: legacy.store})
	if gotCount != wantCount {
		t.Fatalf("targeted count = %d, legacy full-scan count = %d", gotCount, wantCount)
	}
	got := csharpIfaceDispatchSnapshots(targeted.store)
	want := csharpIfaceDispatchSnapshots(legacy.store)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("targeted output differs from legacy full-scan output:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestResolveCSharpInterfaceDispatchFullScopeUsesTargetedIncomingAdjacency(t *testing.T) {
	fixture := newCSharpIfaceDispatchFixture(512)
	spy := &csharpIfaceIncomingSpyStore{Store: fixture.store}

	if got := ResolveCSharpInterfaceDispatch(spy); got != 2 {
		t.Fatalf("synthesized edges = %d, want 2", got)
	}
	if spy.callScans != 0 {
		t.Fatalf("full EdgeCalls scans = %d, want 0", spy.callScans)
	}
	if spy.incomingLookups != 1 {
		t.Fatalf("incoming adjacency lookups = %d, want 1", spy.incomingLookups)
	}

	gotIDs := make(map[string]bool, len(spy.incomingNodeIDs))
	for _, id := range spy.incomingNodeIDs {
		gotIDs[id] = true
	}
	wantIDs := make(map[string]bool, len(fixture.familyMemberIDs))
	for _, id := range fixture.familyMemberIDs {
		wantIDs[id] = true
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) {
		t.Fatalf("incoming adjacency IDs = %#v, want family members %#v", gotIDs, wantIDs)
	}

	var synthesized []csharpIfaceDispatchSnapshot
	for _, edge := range csharpIfaceDispatchSnapshots(fixture.store) {
		if edge.from == fixture.callerID {
			synthesized = append(synthesized, edge)
		}
	}
	if len(synthesized) != 2 {
		t.Fatalf("caller synthesized edges = %#v, want two family siblings", synthesized)
	}
	for _, edge := range synthesized {
		if edge.via != "csharp-iface-dispatch" || edge.iface != "csharp-repo::IRunner" || edge.candidates != "2" {
			t.Fatalf("synthesized edge lost dispatch metadata: %#v", edge)
		}
	}
}
