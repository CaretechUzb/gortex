package languages

import (
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

// declRange finds one declaration by name in a real extraction result.
func declRange(t *testing.T, res *parser.ExtractionResult, name string) (int, int, graph.NodeKind) {
	t.Helper()
	for _, n := range res.Nodes {
		if n.Name == name {
			return n.StartLine, n.EndLine, n.Kind
		}
	}
	t.Fatalf("declaration %q not extracted; got %v", name, extractedNames(res))
	return 0, 0, ""
}

func extractedNames(res *parser.ExtractionResult) []string {
	var out []string
	for _, n := range res.Nodes {
		out = append(out, n.Name)
	}
	return out
}

// A preprocessor definition must not claim the line of the declaration that
// follows it. The grammar includes the terminating newline in the definition
// node, so a naive end-point conversion overshoots by one line and the macro
// becomes the narrowest — and therefore winning — enclosing declaration for
// the next declaration's own signature line.
//
// This drives the real extractors on purpose: a hand-built node range cannot
// exercise the end-point arithmetic under test.
func TestMacroDefinitionDoesNotOwnFollowingDeclarationLine(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		extract func(string, []byte) (*parser.ExtractionResult, error)
		src     string
		macro   string
		next    string
	}{
		{
			name: "c_single_line_define",
			path: "fixture.c",
			extract: func(p string, b []byte) (*parser.ExtractionResult, error) {
				return NewCExtractor().Extract(p, b)
			},
			src:   "#define LIMIT 10\nint compute(void) {\n\treturn LIMIT;\n}\n",
			macro: "LIMIT", next: "compute",
		},
		{
			name: "cpp_single_line_define",
			path: "fixture.cpp",
			extract: func(p string, b []byte) (*parser.ExtractionResult, error) {
				return NewCppExtractor().Extract(p, b)
			},
			src:   "#define LIMIT 10\nint compute() {\n\treturn LIMIT;\n}\n",
			macro: "LIMIT", next: "compute",
		},
		{
			name: "c_multiline_define",
			path: "fixture.c",
			extract: func(p string, b []byte) (*parser.ExtractionResult, error) {
				return NewCExtractor().Extract(p, b)
			},
			src: "#define WRAP(x) do { \\\n\t(x)++; \\\n} while (0)\nint compute(void) {\n\treturn 0;\n}\n",
			macro: "WRAP", next: "compute",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := tc.extract(tc.path, []byte(tc.src))
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			mStart, mEnd, mKind := declRange(t, res, tc.macro)
			if mKind != graph.KindMacro {
				t.Fatalf("%s kind = %s, want macro", tc.macro, mKind)
			}
			nStart, _, _ := declRange(t, res, tc.next)
			if mEnd >= nStart {
				t.Errorf("macro %s [%d,%d] overlaps %s starting at line %d; "+
					"the definition must end on its own last source line",
					tc.macro, mStart, mEnd, tc.next, nStart)
			}
		})
	}
}
