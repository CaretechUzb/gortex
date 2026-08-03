package resolver

import (
	"fmt"
	"iter"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type csharpProjectionlessStore struct {
	graph.Store
}

type csharpMemberScanCountingStore struct {
	*graph.Graph
	memberOfScans int
}

func (s *csharpMemberScanCountingStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	if kind == graph.EdgeMemberOf {
		s.memberOfScans++
	}
	return s.Graph.EdgesByKind(kind)
}

type csharpDispatchSnapshot struct {
	From            string
	To              string
	FilePath        string
	Line            int
	Origin          string
	Confidence      float64
	ConfidenceLabel string
	Via             string
	Synthesizer     string
	IfaceType       string
	CandidateCount  int
}

func TestCSharpFullCensusMemberProjectionMatchesLegacy(t *testing.T) {
	projectedGraph := newCSharpProjectionFixture()
	projected := &csharpMemberScanCountingStore{Graph: projectedGraph}
	projectedCount := ResolveCSharpInterfaceDispatch(projected)
	if projected.memberOfScans != 0 {
		t.Fatalf("full-census projection used %d broad member edge scans", projected.memberOfScans)
	}

	legacyGraph := newCSharpProjectionFixture()
	legacyCount := ResolveCSharpInterfaceDispatch(csharpProjectionlessStore{Store: legacyGraph})
	if projectedCount != legacyCount {
		t.Fatalf("edge count differs: projected=%d legacy=%d", projectedCount, legacyCount)
	}
	if projectedCount != 1 {
		t.Fatalf("projected edge count = %d, want 1", projectedCount)
	}

	projectedEdges := csharpSynthesizedDispatchSnapshots(projectedGraph)
	legacyEdges := csharpSynthesizedDispatchSnapshots(legacyGraph)
	if !reflect.DeepEqual(projectedEdges, legacyEdges) {
		t.Fatalf("projected output differs from legacy:\nprojected=%#v\nlegacy=%#v", projectedEdges, legacyEdges)
	}
}

func newCSharpProjectionFixture() *graph.Graph {
	g := graph.New()
	const (
		repo        = "repo"
		ifaceType   = "repo::IWorker"
		implType    = "repo::Worker"
		ifaceMethod = "repo::IWorker.Run"
		implMethod  = "repo::Worker.Run"
		caller      = "repo::Caller.Invoke"
	)
	nodes := []*graph.Node{
		{ID: ifaceType, Kind: graph.KindInterface, Name: "IWorker", Language: "csharp", FilePath: "IWorker.cs", RepoPrefix: repo},
		{ID: implType, Kind: graph.KindType, Name: "Worker", Language: "csharp", FilePath: "Worker.cs", RepoPrefix: repo},
		{ID: ifaceMethod, Kind: graph.KindMethod, Name: "Run", Language: "csharp", FilePath: "IWorker.cs", StartLine: 2, RepoPrefix: repo, Meta: map[string]any{"iface_member": true}},
		{ID: implMethod, Kind: graph.KindMethod, Name: "Run", Language: "csharp", FilePath: "Worker.cs", StartLine: 4, RepoPrefix: repo},
		{ID: caller, Kind: graph.KindMethod, Name: "Invoke", Language: "csharp", FilePath: "Caller.cs", StartLine: 8, RepoPrefix: repo},
	}
	edges := []*graph.Edge{
		{From: implType, To: ifaceType, Kind: graph.EdgeImplements, FilePath: "Worker.cs", Line: 1},
		{From: ifaceMethod, To: ifaceType, Kind: graph.EdgeMemberOf, FilePath: "IWorker.cs", Line: 2},
		{From: implMethod, To: implType, Kind: graph.EdgeMemberOf, FilePath: "Worker.cs", Line: 4},
		{From: caller, To: ifaceMethod, Kind: graph.EdgeCalls, FilePath: "Caller.cs", Line: 9, Origin: graph.OriginASTResolved, Confidence: 0.9},
	}
	g.AddBatch(nodes, edges)
	return g
}

func csharpSynthesizedDispatchSnapshots(g graph.Store) []csharpDispatchSnapshot {
	var out []csharpDispatchSnapshot
	for edge := range g.EdgesByKind(graph.EdgeCalls) {
		if edge == nil || edge.Meta == nil || edge.Meta[MetaSynthesizedBy] != SynthCSharpIfaceDispatch {
			continue
		}
		count, _ := edge.Meta["candidate_count"].(int)
		out = append(out, csharpDispatchSnapshot{
			From:            edge.From,
			To:              edge.To,
			FilePath:        edge.FilePath,
			Line:            edge.Line,
			Origin:          edge.Origin,
			Confidence:      edge.Confidence,
			ConfidenceLabel: edge.ConfidenceLabel,
			Via:             fmt.Sprint(edge.Meta["via"]),
			Synthesizer:     fmt.Sprint(edge.Meta[MetaSynthesizedBy]),
			IfaceType:       fmt.Sprint(edge.Meta["iface_type"]),
			CandidateCount:  count,
		})
	}
	return out
}

var csharpProjectionBenchmarkSink int

func BenchmarkCSharpFullCensusMemberProjection(b *testing.B) {
	store, err := store_sqlite.Open(filepath.Join(b.TempDir(), "projection.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()

	const rows = 8000
	payload := strings.Repeat("metadata", 256)
	nodes := make([]*graph.Node, 0, rows)
	edges := make([]*graph.Edge, 0, rows)
	for i := 0; i < rows; i++ {
		id := fmt.Sprintf("repo::Type%d.Method", i)
		owner := fmt.Sprintf("repo::Type%d", i)
		nodes = append(nodes, &graph.Node{
			ID: id, Kind: graph.KindMethod, Name: "Method", Language: "go",
			FilePath: "generated.go", RepoPrefix: "repo",
			Meta: map[string]any{"payload": payload},
		})
		edges = append(edges, &graph.Edge{
			From: id, To: owner, Kind: graph.EdgeMemberOf,
			FilePath: "generated.go", Line: i + 1,
			Meta: map[string]any{"payload": payload},
		})
	}
	store.AddBatch(nodes, edges)
	children := map[string][]string{"repo::Interface": {"repo::Impl"}}

	b.Run("projected", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			edges, nodes, _, ok := csharpScopedMemberProjection(store, nil, children)
			if !ok {
				b.Fatal("projection unavailable")
			}
			methods := csharpMemberMethodsAllByTypeFromEdges(edges, nodes)
			csharpProjectionBenchmarkSink = len(edges) + len(nodes) + len(methods)
		}
	})
	b.Run("legacy_full_decode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			edges := frameworkRepoEdges(store, nil, graph.EdgeMemberOf)
			ids := make([]string, 0, len(edges))
			for _, edge := range edges {
				if edge != nil {
					ids = append(ids, edge.From)
				}
			}
			nodes := store.GetNodesByIDs(ids)
			methods := csharpMemberMethodsAllByTypeFromEdges(edges, nodes)
			csharpProjectionBenchmarkSink = len(edges) + len(nodes) + len(methods)
		}
	})
}
