package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

func TestFindEnclosingFuncChoosesDeepestDeclaredCallable(t *testing.T) {
	result := &parser.ExtractionResult{Nodes: []*graph.Node{
		// Deliberately not in lexical order: extraction order is language- and
		// query-dependent and must not decide ownership.
		{ID: "fixture.go::outer#closure@+9", Kind: graph.KindClosure, StartLine: 9, EndLine: 12},
		{ID: "fixture.go::outer", Kind: graph.KindFunction, StartLine: 1, EndLine: 30},
		{ID: "fixture.go::inner", Kind: graph.KindFunction, StartLine: 5, EndLine: 20},
	}}
	ranges := buildFuncRanges(result)
	if len(ranges) != 2 {
		t.Fatalf("line-only callable ranges included an ambiguous closure: %#v", ranges)
	}

	for _, test := range []struct {
		name string
		line int
		want string
	}{
		{name: "outer body", line: 2, want: "fixture.go::outer"},
		{name: "nested function", line: 6, want: "fixture.go::inner"},
		{name: "closure declaration line remains with declared owner", line: 9, want: "fixture.go::inner"},
		{name: "closure body remains with declared owner", line: 10, want: "fixture.go::inner"},
		{name: "after nested function", line: 25, want: "fixture.go::outer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := findEnclosingFunc(ranges, test.line); got != test.want {
				t.Fatalf("findEnclosingFunc(line=%d) = %q, want %q", test.line, got, test.want)
			}
		})
	}
}

func TestFindEnclosingFuncTieIsDeterministic(t *testing.T) {
	ranges := []funcRange{
		{id: "fixture.go::zeta", startLine: 4, endLine: 8},
		{id: "fixture.go::alpha", startLine: 4, endLine: 8},
	}
	if got := findEnclosingFunc(ranges, 6); got != "fixture.go::alpha" {
		t.Fatalf("equal-span owner = %q, want lexical identity tie-break", got)
	}
}
