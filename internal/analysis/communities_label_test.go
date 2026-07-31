package analysis

import (
	"path/filepath"
	"strings"
	"testing"
)

// The cases above spell their inputs with forward slashes, which is what a
// POSIX graph stores — but graphRelKey keeps OS-native separators, so on
// Windows a real graph path reads "gortex/internal\parser\languages\cpp.go":
// the repo prefix is joined with "/" and the repo-relative remainder is not.
// Feeding that shape in is what distinguishes "normalises first" from "happens
// to work because the test used slashes". filepath.FromSlash makes the input
// native on whichever platform runs it, so this is a real assertion on Windows
// and an identity on POSIX rather than a skip.
func TestClusterLabelsNormaliseOSNativeGraphPaths(t *testing.T) {
	// Mirror the "<prefix>/<os-native remainder>" shape graph nodes carry.
	native := func(prefix, rel string) string {
		return prefix + "/" + filepath.FromSlash(rel)
	}
	files := []string{
		native("gortex", "internal/parser/languages/cpp.go"),
		native("gortex", "internal/parser/languages/dart.go"),
		native("gortex", "internal/parser/languages/python.go"),
	}

	if got := pureClusterLabel(files); got != "parser/languages" {
		t.Errorf("pureClusterLabel(%v) = %q; want %q", files, got, "parser/languages")
	}

	// A label must never leak the OS separator to the wire, whichever branch
	// produced it.
	for _, got := range []string{pureClusterLabel(files), mixedClusterLabel(files)} {
		if filepath.Separator != '/' && strings.ContainsRune(got, filepath.Separator) {
			t.Errorf("label %q carries the OS separator", got)
		}
	}

	// Spread across sibling directories: the shared ancestor is still found,
	// so the modal-directory branch reports a real prefix rather than "".
	mixed := []string{
		native("gortex", "internal/parser/languages/cpp.go"),
		native("gortex", "internal/parser/golang/extract.go"),
	}
	if got := longestCommonDirPrefix(mixed); got != "gortex/internal/parser" {
		t.Errorf("longestCommonDirPrefix(%v) = %q; want %q", mixed, got, "gortex/internal/parser")
	}
}

func TestPureClusterLabel(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{
			name: "all files under one deep dir",
			files: []string{
				"gortex/internal/parser/languages/cpp.go",
				"gortex/internal/parser/languages/dart.go",
				"gortex/internal/parser/languages/python.go",
			},
			want: "parser/languages",
		},
		{
			name: "all files under repo + plumbing only",
			files: []string{
				"gortex/internal/parser/extractor.go",
				"gortex/internal/graph/edge.go",
			},
			want: "",
		},
		{
			name: "single file → plumbing stripped from directory",
			files: []string{
				"gortex/internal/server/dashboard.go",
			},
			want: "server",
		},
		{
			name:  "empty",
			files: []string{},
			want:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := pureClusterLabel(tt.files)
			if got != tt.want {
				t.Errorf("pureClusterLabel(%v) = %q; want %q", tt.files, got, tt.want)
			}
		})
	}
}

func TestMixedClusterLabel(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  string
	}{
		{
			name: "spread across many top-level dirs surfaces spread",
			files: []string{
				"gortex/internal/parser/languages/abap.go",
				"gortex/internal/parser/languages/actionscript.go",
				"gortex/internal/parser/languages/ada.go",
				"gortex/internal/parser/extractor.go",
				"gortex/internal/dataflow/dataflow.go",
				"gortex/internal/graph/edge.go",
				"gortex/internal/graph/node.go",
				"gortex/internal/mcp/tools_ast.go",
				"gortex/internal/llm/daemon_backend.go",
			},
			want: "parser/languages +5 dirs",
		},
		{
			name: "two dirs, modal wins with +1 dirs",
			files: []string{
				"gortex/internal/foo/a.go",
				"gortex/internal/foo/b.go",
				"gortex/internal/bar/x.go",
			},
			want: "foo +1 dirs",
		},
		{
			name: "single dir → no spread annotation",
			files: []string{
				"gortex/internal/foo/a.go",
				"gortex/internal/foo/b.go",
			},
			want: "foo",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mixedClusterLabel(tt.files)
			if got != tt.want {
				t.Errorf("mixedClusterLabel = %q; want %q", got, tt.want)
			}
		})
	}
}

func TestDisambiguateLabelsCascadesToSizeSuffix(t *testing.T) {
	// Two clusters share modal dir AND first-file AND second-file —
	// only the size suffix breaks the tie.
	communities := []Community{
		{ID: "a", Label: "server", Size: 50, Files: []string{"server/h.go", "server/m.go"}},
		{ID: "b", Label: "server", Size: 70, Files: []string{"server/h.go", "server/m.go"}},
	}
	disambiguateLabels(communities)
	if communities[0].Label == communities[1].Label {
		t.Fatalf("size tiebreaker failed: both %q", communities[0].Label)
	}
}

func TestDisambiguateLabelsOrdinalTiebreaker(t *testing.T) {
	// Three clusters with the same dir, same first two files, AND
	// the same size — only the ordinal tiebreaker can split them.
	communities := []Community{
		{ID: "a", Label: "mcp", Size: 15, Files: []string{"mcp/memories.go", "mcp/notes.go"}},
		{ID: "b", Label: "mcp", Size: 15, Files: []string{"mcp/memories.go", "mcp/notes.go"}},
		{ID: "c", Label: "mcp", Size: 15, Files: []string{"mcp/memories.go", "mcp/notes.go"}},
	}
	disambiguateLabels(communities)
	labels := map[string]bool{}
	for _, c := range communities {
		if labels[c.Label] {
			t.Fatalf("ordinal tiebreaker failed: duplicate %q", c.Label)
		}
		labels[c.Label] = true
	}
}

func TestDisambiguateLabelsUsesHubFirst(t *testing.T) {
	// Two pure clusters under the same directory should get their
	// hub-symbol names appended — the most meaningful disambiguator.
	// File basenames are only the fallback when the hub is missing.
	communities := []Community{
		{
			ID:    "community-1",
			Label: "parser/languages",
			Hub:   "GoExtractor",
			Files: []string{
				"gortex/internal/parser/languages/cpp.go",
				"gortex/internal/parser/languages/csharp.go",
			},
		},
		{
			ID:    "community-2",
			Label: "parser/languages",
			Hub:   "DartExtractor",
			Files: []string{
				"gortex/internal/parser/languages/dart.go",
				"gortex/internal/parser/languages/flutter.go",
			},
		},
		{
			ID:    "community-3",
			Label: "server",
			Hub:   "RunServer",
			Files: []string{"gortex/internal/server/handler.go"},
		},
	}
	disambiguateLabels(communities)
	wantA := "parser/languages · GoExtractor"
	wantB := "parser/languages · DartExtractor"
	if communities[0].Label != wantA {
		t.Errorf("community[0] = %q; want %q", communities[0].Label, wantA)
	}
	if communities[1].Label != wantB {
		t.Errorf("community[1] = %q; want %q", communities[1].Label, wantB)
	}
	if communities[2].Label != "server" {
		t.Errorf("unique label was modified: %q", communities[2].Label)
	}
}

func TestDisambiguateFallsBackToFileWhenNoHub(t *testing.T) {
	// When the hub is missing (e.g. legacy data, no member with a
	// resolvable name), the cascade falls back to file basenames.
	communities := []Community{
		{
			ID:    "a", Label: "parser/languages", Hub: "",
			Files: []string{"gortex/internal/parser/languages/cpp.go"},
		},
		{
			ID:    "b", Label: "parser/languages", Hub: "",
			Files: []string{"gortex/internal/parser/languages/dart.go"},
		},
	}
	disambiguateLabels(communities)
	if communities[0].Label == communities[1].Label {
		t.Fatalf("fallback failed: both %q", communities[0].Label)
	}
}

func TestCleanHubName(t *testing.T) {
	cases := map[string]string{
		"":                 "",
		"runServer":        "runServer",
		"(*Extractor).Extract":    "Extractor.Extract",
		"(Receiver).Method":       "Receiver.Method",
		"WayTooLongFunctionNameOfAReallyLongType.WithAMethod": "WayTooLongFunctionNameOfAReally…",
	}
	for in, want := range cases {
		got := cleanHubName(in)
		if got != want {
			t.Errorf("cleanHubName(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestInferCommunityLabelDistinguishesSameBasename(t *testing.T) {
	// Two clusters whose basenames collide ("languages") but whose
	// contents differ should produce different labels.
	pureFiles := []string{
		"gortex/internal/parser/languages/cpp.go",
		"gortex/internal/parser/languages/dart.go",
		"gortex/internal/parser/languages/python.go",
	}
	mixedFiles := []string{
		"gortex/internal/parser/languages/abap.go",
		"gortex/internal/parser/extractor.go",
		"gortex/internal/dataflow/dataflow.go",
		"gortex/internal/graph/edge.go",
		"gortex/internal/mcp/tools_ast.go",
	}
	pure := inferCommunityLabel(nil, nil, pureFiles)
	mixed := inferCommunityLabel(nil, nil, mixedFiles)
	if pure == mixed {
		t.Fatalf("two structurally distinct clusters share label %q (regression — naming collision is back)", pure)
	}
	if pure != "parser/languages" {
		t.Errorf("pure cluster label = %q; want %q", pure, "parser/languages")
	}
	if mixed == "languages" || mixed == "parser/languages" {
		t.Errorf("mixed cluster label = %q; should surface its spread", mixed)
	}
}
