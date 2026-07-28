package analysis

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// coveredSymbolGraph wires one production symbol exercised by one test at
// testPath, in the shape the indexer produces: a calls edge plus the parallel
// tests edge its test-edge pass emits.
func coveredSymbolGraph(prodPath, testPath string) (graph.Store, string) {
	g := graph.New()
	prodID := prodPath + "::Target"
	testID := testPath + "::Exercise"
	g.AddNode(&graph.Node{ID: prodID, Name: "Target", Kind: graph.KindFunction, FilePath: prodPath})
	g.AddNode(&graph.Node{
		ID: testID, Name: "Exercise", Kind: graph.KindFunction, FilePath: testPath,
		Meta: map[string]any{"is_test": true},
	})
	g.AddEdge(&graph.Edge{From: testID, To: prodID, Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: testID, To: prodID, Kind: graph.EdgeTests})
	return g, prodID
}

// TestCoverageVocabulariesAgree is the regression for issue #350's second
// contradiction: the per-file rows counted a symbol covered when a dependent
// looked like a test file, while the risk receipt counted it covered when a
// tests edge pointed at it. The path predicate recognised seven filename
// fragments, so every convention outside that list — a Ruby spec, a JUnit
// class, anything under a tests/ directory — was covered by one measure and
// uncovered by the other in the same payload.
func TestCoverageVocabulariesAgree(t *testing.T) {
	conventions := []struct{ name, prod, test string }{
		{"go", "pkg/foo.go", "pkg/foo_test.go"},
		{"vitest colocated", "src/entity.ts", "src/entity.test.ts"},
		{"ruby spec", "app/user.rb", "spec/user_spec.rb"},
		{"junit", "src/User.java", "src/UserTest.java"},
		{"tests directory", "src/parse.rs", "tests/parse_integration.rs"},
		{"pytest", "app/service.py", "app/test_service.py"},
		{"phpunit", "src/Order.php", "tests/OrderTest.php"},
	}
	for _, c := range conventions {
		t.Run(c.name, func(t *testing.T) {
			g, prodID := coveredSymbolGraph(c.prod, c.test)

			impact := AnalyzeImpact(g, []string{prodID}, nil, nil)
			require.NotNil(t, impact)
			require.Contains(t, impact.TestFiles, c.test,
				"the per-file review rows read coverage off TestFiles")

			require.True(t, hasCoveringTest(g, prodID),
				"the risk receipt reads coverage off the tests edge")
			require.Zero(t, ScorePRRisk(g, PRRiskInput{SymbolIDs: []string{prodID}}).UncoveredSymbols)
		})
	}
}

// TestCoverageDoesNotCountTestSubstringPaths pins the false positive the old
// path predicate carried: it matched "test_" anywhere in a path, so an
// ordinary caller whose name merely contains the substring inflated a
// symbol's covering-test list.
func TestCoverageDoesNotCountTestSubstringPaths(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "pkg/foo.go::Target", Name: "Target", Kind: graph.KindFunction, FilePath: "pkg/foo.go"})
	g.AddNode(&graph.Node{ID: "pkg/latest_release.go::Publish", Name: "Publish", Kind: graph.KindFunction, FilePath: "pkg/latest_release.go"})
	g.AddEdge(&graph.Edge{From: "pkg/latest_release.go::Publish", To: "pkg/foo.go::Target", Kind: graph.EdgeCalls})

	impact := AnalyzeImpact(g, []string{"pkg/foo.go::Target"}, nil, nil)
	require.NotNil(t, impact)
	require.Empty(t, impact.TestFiles, "a production caller is not a covering test")
}
