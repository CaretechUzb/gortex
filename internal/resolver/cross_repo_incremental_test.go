package resolver

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestDetectCrossRepoEdgesForFilesUsesExactIncidentFrontier(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "a/a.go::Call", Kind: graph.KindFunction, Name: "Call", FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "b/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b/b.go", RepoPrefix: "b"},
		{ID: "b/b.go::Serve", Kind: graph.KindFunction, Name: "Serve", FilePath: "b/b.go", RepoPrefix: "b"},
		{ID: "c/c.go", Kind: graph.KindFile, Name: "c.go", FilePath: "c/c.go", RepoPrefix: "c"},
		{ID: "c/c.go::Call", Kind: graph.KindFunction, Name: "Call", FilePath: "c/c.go", RepoPrefix: "c"},
		{ID: "d/d.go", Kind: graph.KindFile, Name: "d.go", FilePath: "d/d.go", RepoPrefix: "d"},
		{ID: "d/d.go::Serve", Kind: graph.KindFunction, Name: "Serve", FilePath: "d/d.go", RepoPrefix: "d"},
	}, []*graph.Edge{
		{From: "a/a.go::Call", To: "b/b.go::Serve", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 3},
		{From: "c/c.go::Call", To: "d/d.go::Serve", Kind: graph.EdgeCalls, FilePath: "c/c.go", Line: 4},
	})

	if got := DetectCrossRepoEdgesForFiles(g, []string{"b/b.go"}); got != 1 {
		t.Fatalf("emitted = %d, want incident edge only", got)
	}
	crossKind, ok := graph.CrossRepoKindFor(graph.EdgeCalls)
	if !ok {
		t.Fatal("calls has no cross-repo kind")
	}
	if !hasEdgeKindTo(g.GetOutEdges("a/a.go::Call"), crossKind, "b/b.go::Serve") {
		t.Fatal("incoming edge to changed target was not materialized")
	}
	if hasEdgeKindTo(g.GetOutEdges("c/c.go::Call"), crossKind, "d/d.go::Serve") {
		t.Fatal("unrelated cross-repo edge leaked outside exact frontier")
	}
}

func TestDetectCrossRepoEdgesForMutationFilesSeparatesRoles(t *testing.T) {
	newFixture := func() *graph.Graph {
		g := graph.New()
		nodes := []*graph.Node{
			{ID: "repoA/a.go::A", Kind: graph.KindFunction, FilePath: "repoA/a.go", RepoPrefix: "repoA"},
			{ID: "repoB/b.go::B", Kind: graph.KindFunction, FilePath: "repoB/b.go", RepoPrefix: "repoB"},
			{ID: "repoD/definition.go::D", Kind: graph.KindFunction, FilePath: "repoD/definition.go", RepoPrefix: "repoD"},
			{ID: "repoE/e.go::E", Kind: graph.KindFunction, FilePath: "repoE/e.go", RepoPrefix: "repoE"},
			{ID: "repoF/edge-source.go::F", Kind: graph.KindFunction, FilePath: "repoF/edge-source.go", RepoPrefix: "repoF"},
			{ID: "repoG/g.go::G", Kind: graph.KindFunction, FilePath: "repoG/g.go", RepoPrefix: "repoG"},
		}
		g.AddBatch(nodes, []*graph.Edge{
			{From: nodes[0].ID, To: nodes[1].ID, Kind: graph.EdgeCalls, FilePath: "repoF/edge-source.go", Line: 1},
			{From: nodes[4].ID, To: nodes[5].ID, Kind: graph.EdgeCalls, FilePath: "elsewhere.go", Line: 2},
			{From: nodes[2].ID, To: nodes[3].ID, Kind: graph.EdgeCalls, FilePath: "elsewhere.go", Line: 3},
			{From: nodes[0].ID, To: nodes[1].ID, Kind: graph.EdgeCalls, FilePath: "repoD/definition.go", Line: 4},
		})
		return g
	}
	crossKind, ok := graph.CrossRepoKindFor(graph.EdgeCalls)
	if !ok {
		t.Fatal("calls has no cross-repo kind")
	}

	t.Run("edge source", func(t *testing.T) {
		g := newFixture()
		if got := DetectCrossRepoEdgesForMutationFiles(g, []string{"repoF/edge-source.go"}, nil); got != 1 {
			t.Fatalf("emitted = %d, want edge-source edge only", got)
		}
		if !hasEdgeKindTo(g.GetOutEdges("repoA/a.go::A"), crossKind, "repoB/b.go::B") {
			t.Fatal("edge stamped with changed source file was not materialized")
		}
		if hasEdgeKindTo(g.GetOutEdges("repoF/edge-source.go::F"), crossKind, "repoG/g.go::G") {
			t.Fatal("edge-source role broadened into endpoint incidence")
		}
	})

	t.Run("definition", func(t *testing.T) {
		g := newFixture()
		if got := DetectCrossRepoEdgesForMutationFiles(g, nil, []string{"repoD/definition.go"}); got != 1 {
			t.Fatalf("emitted = %d, want definition-incident edge only", got)
		}
		if !hasEdgeKindTo(g.GetOutEdges("repoD/definition.go::D"), crossKind, "repoE/e.go::E") {
			t.Fatal("edge incident to changed definition was not materialized")
		}
		if hasEdgeKindTo(g.GetOutEdges("repoA/a.go::A"), crossKind, "repoB/b.go::B") {
			t.Fatal("definition role broadened into edge source stamps")
		}
	})
}

func TestResolveForFilePrefixesMultiRepoGraphPath(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "a/a.go::Call", Kind: graph.KindFunction, Name: "Call", FilePath: "a/a.go", RepoPrefix: "a"},
		{ID: "b/b.go", Kind: graph.KindFile, Name: "b.go", FilePath: "b/b.go", RepoPrefix: "b"},
		{ID: "b/b.go::Serve", Kind: graph.KindFunction, Name: "Serve", FilePath: "b/b.go", RepoPrefix: "b"},
	}, []*graph.Edge{
		{From: "a/a.go::Call", To: "b/b.go::Serve", Kind: graph.EdgeCalls, FilePath: "a/a.go", Line: 3},
	})

	NewCrossRepo(g).ResolveForFile("a", "a.go")

	crossKind, ok := graph.CrossRepoKindFor(graph.EdgeCalls)
	if !ok {
		t.Fatal("calls has no cross-repo kind")
	}
	if !hasEdgeKindTo(g.GetOutEdges("a/a.go::Call"), crossKind, "b/b.go::Serve") {
		t.Fatal("repo-relative watcher path did not resolve to its prefixed graph file")
	}
}

func hasEdgeKindTo(edges []*graph.Edge, kind graph.EdgeKind, target string) bool {
	for _, edge := range edges {
		if edge != nil && edge.Kind == kind && edge.To == target {
			return true
		}
	}
	return false
}
