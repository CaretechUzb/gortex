package mcp

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestFileSymbolIndexMapsInteriorMacroLinesToDeclaration(t *testing.T) {
	g := graph.New()
	for _, node := range []*graph.Node{
		{ID: "fixture.c::configure", Name: "configure", Kind: graph.KindFunction, FilePath: "fixture.c", StartLine: 1, EndLine: 10},
		{ID: "fixture.c::APPLY", Name: "APPLY", Kind: graph.KindMacro, FilePath: "fixture.c", StartLine: 2, EndLine: 4},
		{ID: "fixture.rs::apply", Name: "apply", Kind: graph.KindMacro, FilePath: "fixture.rs", StartLine: 20, EndLine: 24},
	} {
		g.AddNode(node)
	}

	server := &Server{graph: g}
	indexes := server.buildFileSymbolIndexForPaths(map[string]struct{}{
		"fixture.c":  {},
		"fixture.rs": {},
	})
	for _, test := range []struct {
		name string
		file string
		line int
		id   string
	}{
		{name: "multiline C macro", file: "fixture.c", line: 3, id: "fixture.c::APPLY"},
		{name: "Rust macro rule body", file: "fixture.rs", line: 22, id: "fixture.rs::apply"},
	} {
		t.Run(test.name, func(t *testing.T) {
			index := indexes[test.file]
			if index == nil {
				t.Fatalf("no symbol index for %s", test.file)
			}
			if got, _ := index.find(test.line); got != test.id {
				t.Fatalf("enclosing symbol at %s:%d = %q, want %q", test.file, test.line, got, test.id)
			}
		})
	}
}

func TestFileSymbolIndexEqualRangesUseDeterministicIdentityTie(t *testing.T) {
	index := &fileSymbolIndex{}
	// Reverse lexical insertion order to model graph-map iteration.
	index.add(&graph.Node{ID: "fixture.rs::zeta", Name: "zeta", Kind: graph.KindMacro, StartLine: 7, EndLine: 7})
	index.add(&graph.Node{ID: "fixture.rs::alpha", Name: "alpha", Kind: graph.KindMacro, StartLine: 7, EndLine: 7})
	index.finalise()

	if got, _ := index.find(7); got != "fixture.rs::alpha" {
		t.Fatalf("equal-range owner = %q, want canonical identity tie-break", got)
	}
}
