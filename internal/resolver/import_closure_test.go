package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Every member of a barrel cycle must expose every reachable directory,
// regardless of which import the graph iterator visits first. Each barrel
// has its own leaf so either cache-population order exposes the bug.
func TestBuildImportClosure_CyclicBarrels(t *testing.T) {
	for _, scoped := range []bool{false, true} {
		name := "full"
		if scoped {
			name = "scoped_cross_repo"
		}
		t.Run(name, func(t *testing.T) {
			g := graph.New()
			files := []string{
				"caller/first/main.ts", "caller/second/main.ts",
				"barrels/a/index.ts", "barrels/b/index.ts",
				"leaves/x/value.ts", "leaves/y/value.ts",
			}
			for _, file := range files {
				g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, Name: file,
					FilePath: file, Language: "typescript", RepoPrefix: graph.RepoPrefixOfID(file)})
			}
			addEdge := func(from, to string, kind graph.EdgeKind) {
				g.AddEdge(&graph.Edge{From: from, To: to, Kind: kind,
					FilePath: from, Line: 1, Origin: graph.OriginASTResolved})
			}
			addEdge(files[0], files[2], graph.EdgeImports)
			addEdge(files[1], files[3], graph.EdgeImports)
			addEdge(files[2], files[3], graph.EdgeReExports)
			addEdge(files[3], files[2], graph.EdgeReExports)
			addEdge(files[2], files[4], graph.EdgeReExports)
			addEdge(files[3], files[5], graph.EdgeReExports)

			r := New(g)
			var repos map[string]struct{}
			if scoped {
				repos = map[string]struct{}{"caller": {}}
			}
			closure := r.buildImportClosureFiltered(repos)
			for _, caller := range files[:2] {
				for _, dir := range []string{"barrels/a", "barrels/b", "leaves/x", "leaves/y"} {
					if _, ok := closure[caller][dir]; !ok {
						t.Errorf("closure[%q] is missing reachable directory %q: %v", caller, dir, closure[caller])
					}
				}
			}
			if scoped {
				if _, ok := closure[files[2]]; ok {
					t.Error("scoped closure seeded an out-of-scope barrel")
				}
			}
		})
	}
}

// Layered diamonds contain only linearly many files but exponentially many
// paths. Benchmark allocations to catch duplicate transitive path expansion.
func BenchmarkBuildImportClosure_LayeredDiamonds(b *testing.B) {
	for _, depth := range []int{12, 16} {
		b.Run(fmt.Sprintf("depth_%d", depth), func(b *testing.B) {
			g := graph.New()
			root := "repo/caller/main.ts"
			addFile := func(file string) {
				g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, Name: file, FilePath: file, Language: "typescript", RepoPrefix: "repo"})
			}
			addEdge := func(from, to string, kind graph.EdgeKind) {
				g.AddEdge(&graph.Edge{From: from, To: to, Kind: kind, FilePath: from, Line: 1, Origin: graph.OriginASTResolved})
			}
			file := func(layer, branch int) string { return fmt.Sprintf("repo/layer%d/branch%d/index.ts", layer, branch) }
			addFile(root)
			for layer := 0; layer < depth; layer++ {
				for branch := 0; branch < 2; branch++ {
					addFile(file(layer, branch))
				}
			}
			for branch := 0; branch < 2; branch++ {
				addEdge(root, file(0, branch), graph.EdgeImports)
			}
			for layer := 0; layer < depth-1; layer++ {
				for branch := 0; branch < 2; branch++ {
					for next := 0; next < 2; next++ {
						addEdge(file(layer, branch), file(layer+1, next), graph.EdgeReExports)
					}
				}
			}
			r := New(g)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				closure := r.buildImportClosure()
				if got := len(closure[root]); got != 2*depth+1 {
					b.Fatalf("got %d reachable directories, want %d", got, 2*depth+1)
				}
			}
		})
	}
}

func TestBuildImportClosure_ReExportTraversal(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
		edges [][2]string
		want  []string
	}{
		{name: "chain", roots: []string{"repo/a/index.ts"}, edges: [][2]string{{"repo/a/index.ts", "repo/b/index.ts"}, {"repo/b/index.ts", "repo/c/value.ts"}}, want: []string{"repo/a", "repo/b", "repo/c"}},
		{name: "self_loop", roots: []string{"repo/a/index.ts"}, edges: [][2]string{{"repo/a/index.ts", "repo/a/index.ts"}}, want: []string{"repo/a"}},
		{name: "shared_directory", roots: []string{"repo/barrels/a.ts", "repo/barrels/b.ts"}, edges: [][2]string{{"repo/barrels/a.ts", "repo/x/value.ts"}, {"repo/barrels/b.ts", "repo/y/value.ts"}}, want: []string{"repo/barrels", "repo/x", "repo/y"}},
		{name: "diamond", roots: []string{"repo/a/index.ts"}, edges: [][2]string{{"repo/a/index.ts", "repo/b/index.ts"}, {"repo/a/index.ts", "repo/c/index.ts"}, {"repo/b/index.ts", "repo/d/value.ts"}, {"repo/c/index.ts", "repo/d/value.ts"}}, want: []string{"repo/a", "repo/b", "repo/c", "repo/d"}},
		{name: "repeated_target", roots: []string{"repo/a/index.ts"}, edges: [][2]string{{"repo/a/index.ts", "repo/b/value.ts"}, {"repo/a/index.ts", "repo/b/value.ts"}}, want: []string{"repo/a", "repo/b"}},
		{name: "direct_import", roots: []string{"repo/a/value.ts"}, want: []string{"repo/a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := graph.New()
			caller := "repo/caller/main.ts"
			files := map[string]struct{}{caller: {}}
			for _, root := range tt.roots {
				files[root] = struct{}{}
			}
			for _, edge := range tt.edges {
				files[edge[0]] = struct{}{}
				files[edge[1]] = struct{}{}
			}
			for file := range files {
				g.AddNode(&graph.Node{ID: file, Kind: graph.KindFile, Name: file, FilePath: file, Language: "typescript", RepoPrefix: "repo"})
			}
			for i, root := range tt.roots {
				g.AddEdge(&graph.Edge{From: caller, To: root, Kind: graph.EdgeImports, FilePath: caller, Line: i + 1, Origin: graph.OriginASTResolved})
			}
			for i, edge := range tt.edges {
				g.AddEdge(&graph.Edge{From: edge[0], To: edge[1], Kind: graph.EdgeReExports, FilePath: edge[0], Line: i + 1, Origin: graph.OriginASTResolved})
			}
			closure := New(g).buildImportClosure()[caller]
			if len(closure) != len(tt.want)+1 {
				t.Errorf("unexpected closure: %v", closure)
			}
			for _, dir := range append(tt.want, "repo/caller") {
				if _, ok := closure[dir]; !ok {
					t.Errorf("missing directory %q: %v", dir, closure)
				}
			}
		})
	}
}
