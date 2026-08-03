package resolver

import (
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser/languages"
)

// BenchmarkResolveFileAndIncomingNoPending100K guards the watcher hot path:
// a generated asset with no unresolved forward/incoming edge must stay scoped
// to that file instead of rebuilding pass indexes over the whole graph.
func BenchmarkResolveFileAndIncomingNoPending100K(b *testing.B) {
	g := graph.New()
	for i := 0; i < 100_000; i++ {
		path := fmt.Sprintf("pkg/%06d.go", i)
		g.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path, Language: "go"})
	}
	g.AddNode(&graph.Node{ID: "results.json", Kind: graph.KindFile, Name: "results.json", FilePath: "results.json", Language: "json"})
	g.AddNode(&graph.Node{ID: "results.json::records", Kind: graph.KindVariable, Name: "records", FilePath: "results.json", Language: "json"})
	g.AddEdge(&graph.Edge{From: "results.json", To: "results.json::records", Kind: graph.EdgeDefines, FilePath: "results.json"})

	r := New(g)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stats := r.ResolveFileAndIncoming("results.json")
		if stats.Resolved != 0 || stats.Unresolved != 0 {
			b.Fatalf("unexpected resolver work: %+v", stats)
		}
	}
}

// BenchmarkResolveFileAndIncomingCSharpVisibility100K exercises the save
// path the no-pending benchmark above never reaches: a C# member call
// whose ambiguity keeps it pending, so every save re-enters extension
// narrowing and the global-using visibility lookup. Guards the R5
// contract — the per-save cost must not include an O(all files)
// global-using index rebuild (one seed scan, then reconcile).
func BenchmarkResolveFileAndIncomingCSharpVisibility100K(b *testing.B) {
	g := graph.New()
	for i := 0; i < 100_000; i++ {
		path := fmt.Sprintf("pkg/%06d.go", i)
		g.AddNode(&graph.Node{ID: path, Kind: graph.KindFile, Name: path, FilePath: path, Language: "go"})
	}
	e := languages.NewCSharpExtractor()
	for path, src := range map[string]string{
		"W.cs": "namespace W {\n    public class Crate {}\n}\n",
		"E1.cs": "using W;\nnamespace LibA {\n" +
			"    public static class E1 { public static void Foo(this Crate c) {} }\n}\n",
		"E2.cs": "using W;\nnamespace LibB {\n" +
			"    public static class E2 { public static void Foo(this Crate c) {} }\n}\n",
		"Usings.cs": "global using LibX;\n",
		"Caller.cs": "using W;\nnamespace App {\n    public class Runner {\n" +
			"        public void Run() {\n            Crate c = new Crate();\n            c.Foo();\n        }\n    }\n}\n",
	} {
		res, err := e.Extract(path, []byte(src))
		if err != nil {
			b.Fatalf("csharp extract %s: %v", path, err)
		}
		for _, n := range res.Nodes {
			g.AddNode(n)
		}
		for _, ed := range res.Edges {
			g.AddEdge(ed)
		}
	}

	r := New(g)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.ResolveFileAndIncoming("Caller.cs")
	}
}
