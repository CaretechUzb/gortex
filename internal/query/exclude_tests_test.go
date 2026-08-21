package query

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// TestFindUsagesScoped_ExcludeTestsCoversUnstampedKinds pins the
// exclude_tests contract for node kinds the indexer's test-edge pass
// never stamps. The pass writes Meta["is_test"] on function/method
// symbols only — a file-level from-node under a test directory carries
// is_test_file instead, and a parameter node carries no flag at all —
// so a filter that trusts the stamp alone leaks exactly those kinds
// into a "production-only" answer while correctly dropping the stamped
// method callers. The graph below mirrors that indexer output shape.
func TestFindUsagesScoped_ExcludeTestsCoversUnstampedKinds(t *testing.T) {
	g := graph.New()
	target := &graph.Node{
		ID: "src/WidgetService.cs::WidgetService", Kind: graph.KindType,
		Name: "WidgetService", FilePath: "src/WidgetService.cs", StartLine: 5,
	}
	prodCaller := &graph.Node{
		ID: "src/Consumer.cs::Consumer.Use", Kind: graph.KindMethod,
		Name: "Use", FilePath: "src/Consumer.cs", StartLine: 12,
	}
	// Stamped test method — the case the filter already handles.
	testMethod := &graph.Node{
		ID: `Test\WidgetService_Tests.cs::WidgetServiceTests.Run`, Kind: graph.KindMethod,
		Name: "Run", FilePath: `Test\WidgetService_Tests.cs`, StartLine: 20,
		Meta: map[string]any{"is_test": true},
	}
	// File-level from-node: the pass stamps is_test_file, never is_test.
	testFile := &graph.Node{
		ID: `Test\WidgetService_Tests.cs`, Kind: graph.KindFile,
		Name: "WidgetService_Tests.cs", FilePath: `Test\WidgetService_Tests.cs`,
		Meta: map[string]any{"is_test_file": true},
	}
	// Parameter node: no test metadata of any kind.
	testParam := &graph.Node{
		ID: `Test\WidgetService_Tests.cs::WidgetServiceTests.Run#param:svc`, Kind: graph.KindParam,
		Name: "svc", FilePath: `Test\WidgetService_Tests.cs`, StartLine: 20,
	}
	for _, n := range []*graph.Node{target, prodCaller, testMethod, testFile, testParam} {
		g.AddNode(n)
	}
	g.AddEdge(&graph.Edge{From: prodCaller.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: "src/Consumer.cs", Line: 14})
	g.AddEdge(&graph.Edge{From: testMethod.ID, To: target.ID, Kind: graph.EdgeCalls, FilePath: testMethod.FilePath, Line: 22})
	g.AddEdge(&graph.Edge{From: testFile.ID, To: target.ID, Kind: graph.EdgeImports, FilePath: testFile.FilePath, Line: 1})
	g.AddEdge(&graph.Edge{From: testParam.ID, To: target.ID, Kind: graph.EdgeReferences, FilePath: testParam.FilePath, Line: 20})

	eng := NewEngine(g)
	sg := eng.FindUsagesScoped(target.ID, QueryOptions{ExcludeTests: true})

	require.Len(t, sg.Edges, 1, "exclude_tests must drop every test-path from-node, not just stamped methods")
	require.Equal(t, prodCaller.ID, sg.Edges[0].From, "the production caller is the only edge that may survive")
	for _, n := range sg.Nodes {
		require.NotEqual(t, testFile.ID, n.ID, "test file node leaked into a production-only result")
		require.NotEqual(t, testParam.ID, n.ID, "test param node leaked into a production-only result")
	}
}
