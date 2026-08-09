package query

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// buildTestGraph creates a small realistic graph:
//
//	main.go::main -> calls -> pkg/server.go::Start
//	pkg/server.go::Start -> calls -> pkg/db.go::Connect
//	pkg/db.go::Connect -> calls -> pkg/db.go::Ping
//	pkg/server.go -> imports -> pkg/db.go
//	pkg/db.go::DBImpl -> implements -> pkg/db.go::DB (interface)
func buildTestGraph() *graph.Graph {
	g := graph.New()

	nodes := []*graph.Node{
		{ID: "main.go", Kind: graph.KindFile, Name: "main.go", FilePath: "main.go", Language: "go"},
		{ID: "main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "main.go", Language: "go",
			StartLine: 5, EndLine: 10, Meta: map[string]any{"signature": "func main()"}},
		{ID: "pkg/server.go", Kind: graph.KindFile, Name: "server.go", FilePath: "pkg/server.go", Language: "go"},
		{ID: "pkg/server.go::Start", Kind: graph.KindFunction, Name: "Start", FilePath: "pkg/server.go", Language: "go",
			StartLine: 10, EndLine: 20},
		{ID: "pkg/db.go", Kind: graph.KindFile, Name: "db.go", FilePath: "pkg/db.go", Language: "go"},
		{ID: "pkg/db.go::Connect", Kind: graph.KindFunction, Name: "Connect", FilePath: "pkg/db.go", Language: "go",
			StartLine: 5, EndLine: 15},
		{ID: "pkg/db.go::Ping", Kind: graph.KindFunction, Name: "Ping", FilePath: "pkg/db.go", Language: "go",
			StartLine: 17, EndLine: 22},
		{ID: "pkg/db.go::DB", Kind: graph.KindInterface, Name: "DB", FilePath: "pkg/db.go", Language: "go",
			StartLine: 1, EndLine: 4},
		{ID: "pkg/db.go::DBImpl", Kind: graph.KindType, Name: "DBImpl", FilePath: "pkg/db.go", Language: "go",
			StartLine: 24, EndLine: 30},
	}
	for _, n := range nodes {
		g.AddNode(n)
	}

	edges := []*graph.Edge{
		{From: "main.go::main", To: "pkg/server.go::Start", Kind: graph.EdgeCalls, FilePath: "main.go", Line: 7},
		{From: "pkg/server.go::Start", To: "pkg/db.go::Connect", Kind: graph.EdgeCalls, FilePath: "pkg/server.go", Line: 12},
		{From: "pkg/db.go::Connect", To: "pkg/db.go::Ping", Kind: graph.EdgeCalls, FilePath: "pkg/db.go", Line: 10},
		{From: "pkg/server.go", To: "pkg/db.go", Kind: graph.EdgeImports, FilePath: "pkg/server.go", Line: 3},
		{From: "pkg/db.go::DBImpl", To: "pkg/db.go::DB", Kind: graph.EdgeImplements, FilePath: "pkg/db.go", Line: 24},
		{From: "main.go::main", To: "pkg/db.go::DBImpl", Kind: graph.EdgeReferences, FilePath: "main.go", Line: 8},
	}
	for _, e := range edges {
		g.AddEdge(e)
	}

	return g
}

func TestGetSymbol(t *testing.T) {
	e := NewEngine(buildTestGraph())
	n := e.GetSymbol("pkg/db.go::Connect")
	require.NotNil(t, n)
	assert.Equal(t, "Connect", n.Name)

	assert.Nil(t, e.GetSymbol("nonexistent"))
}

func TestFindSymbols(t *testing.T) {
	e := NewEngine(buildTestGraph())

	results := e.FindSymbols("Connect")
	require.Len(t, results, 1)

	results = e.FindSymbols("Connect", graph.KindFunction)
	require.Len(t, results, 1)

	results = e.FindSymbols("Connect", graph.KindType)
	assert.Empty(t, results)
}

func TestGetFileSymbols(t *testing.T) {
	e := NewEngine(buildTestGraph())
	sg := e.GetFileSymbols("pkg/db.go")
	assert.GreaterOrEqual(t, len(sg.Nodes), 4) // file + Connect + Ping + DB + DBImpl
}

func TestGetDependencies(t *testing.T) {
	e := NewEngine(buildTestGraph())

	// main calls Start (depth 1).
	sg := e.GetDependencies("main.go::main", QueryOptions{Depth: 1, Limit: 50, Detail: "full"})
	assert.GreaterOrEqual(t, len(sg.Nodes), 2) // main + Start

	// Depth 2 should also reach Connect.
	sg = e.GetDependencies("main.go::main", QueryOptions{Depth: 2, Limit: 50, Detail: "full"})
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "pkg/server.go::Start")
	assert.Contains(t, ids, "pkg/db.go::Connect")
}

func TestGetDependents(t *testing.T) {
	e := NewEngine(buildTestGraph())

	// Who depends on Connect?
	sg := e.GetDependents("pkg/db.go::Connect", QueryOptions{Depth: 2, Limit: 50})
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "pkg/server.go::Start")
}

func TestGetCallChain(t *testing.T) {
	e := NewEngine(buildTestGraph())

	sg := e.GetCallChain("main.go::main", QueryOptions{Depth: 3, Limit: 50})
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "pkg/server.go::Start")
	assert.Contains(t, ids, "pkg/db.go::Connect")
	assert.Contains(t, ids, "pkg/db.go::Ping")
}

func TestGetCallers(t *testing.T) {
	e := NewEngine(buildTestGraph())

	sg := e.GetCallers("pkg/db.go::Ping", QueryOptions{Depth: 3, Limit: 50})
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "pkg/db.go::Connect")
}

// Regression: methods wired in by reference (e.g. HTTP handler registration
// like `mux.HandleFunc("/p", h.foo)`) carry an EdgeReferences edge from the
// registration site to the method — they're never reached by a direct
// EdgeCalls. Before the fix GetCallers ignored EdgeReferences and these
// methods looked dead in the graph.
func TestGetCallers_IncludesMethodValueReferences(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "routes.go::Register", Kind: graph.KindFunction, Name: "Register", FilePath: "routes.go", Language: "go"})
	g.AddNode(&graph.Node{
		ID: "handler.go::Handler.HandleHealth", Kind: graph.KindMethod, Name: "HandleHealth",
		FilePath: "handler.go", Language: "go", Meta: map[string]any{"receiver": "Handler"},
	})
	// `mux.HandleFunc("/health", h.HandleHealth)` after the resolver:
	g.AddEdge(&graph.Edge{
		From: "routes.go::Register", To: "handler.go::Handler.HandleHealth",
		Kind: graph.EdgeReferences, FilePath: "routes.go", Line: 12,
	})

	e := NewEngine(g)
	sg := e.GetCallers("handler.go::Handler.HandleHealth", QueryOptions{Depth: 1, Limit: 50})
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "routes.go::Register",
		"method-value registration site should surface as a caller via EdgeReferences")
}

func TestFindImplementations(t *testing.T) {
	e := NewEngine(buildTestGraph())

	impls := e.FindImplementations("pkg/db.go::DB")
	require.Len(t, impls, 1)
	assert.Equal(t, "DBImpl", impls[0].Name)
}

// The implements-edges behind the node list must be retrievable too —
// find_implementations' wire format promises a `.edges` section
// (from/to/kind/origin/confidence), and the handler can only fill it
// if the engine hands the edges over instead of dropping them after
// the walk.
func TestFindImplementationsWithEdges(t *testing.T) {
	e := NewEngine(buildTestGraph())

	impls, edges := e.FindImplementationsWithEdgesMinTier("pkg/db.go::DB", "")
	require.Len(t, impls, 1)
	require.Len(t, edges, 1, "one implements edge backs the one implementation")
	assert.Equal(t, impls[0].ID, edges[0].From)
	assert.Equal(t, "pkg/db.go::DB", edges[0].To)
	assert.Equal(t, graph.EdgeImplements, edges[0].Kind)
}

func TestFindUsages(t *testing.T) {
	e := NewEngine(buildTestGraph())

	sg := e.FindUsages("pkg/db.go::DBImpl")
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "main.go::main") // references DBImpl
}

func TestGetCluster(t *testing.T) {
	e := NewEngine(buildTestGraph())

	sg := e.GetCluster("pkg/server.go::Start", QueryOptions{Depth: 1, Limit: 50})
	assert.GreaterOrEqual(t, len(sg.Nodes), 3) // Start + main + Connect
}

func TestTruncation(t *testing.T) {
	e := NewEngine(buildTestGraph())

	sg := e.GetCallChain("main.go::main", QueryOptions{Depth: 10, Limit: 2})
	assert.True(t, sg.Truncated)
	assert.LessOrEqual(t, len(sg.Nodes), 2)
}

func TestCycleHandling(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "a", Kind: graph.KindFunction, Name: "a", FilePath: "a.go", Language: "go"})
	g.AddNode(&graph.Node{ID: "b", Kind: graph.KindFunction, Name: "b", FilePath: "b.go", Language: "go"})
	g.AddEdge(&graph.Edge{From: "a", To: "b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1})
	g.AddEdge(&graph.Edge{From: "b", To: "a", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 1})

	e := NewEngine(g)
	sg := e.GetCallChain("a", QueryOptions{Depth: 10, Limit: 50})
	// Should terminate without infinite loop.
	assert.LessOrEqual(t, len(sg.Nodes), 2)
}

func TestStats(t *testing.T) {
	e := NewEngine(buildTestGraph())
	s := e.Stats()
	assert.Equal(t, 9, s.TotalNodes)
	assert.Equal(t, 6, s.TotalEdges)
	assert.Equal(t, 9, s.ByLanguage["go"])
}

func TestBriefModeStripsMeta(t *testing.T) {
	e := NewEngine(buildTestGraph())
	sg := e.GetDependencies("main.go::main", QueryOptions{Depth: 1, Limit: 50, Detail: "brief"})
	for _, n := range sg.Nodes {
		assert.Nil(t, n.Meta)
	}
}

// buildMultiRepoTestGraph creates a graph with nodes from two repos for testing repo-scoped queries.
func buildMultiRepoTestGraph() *graph.Graph {
	g := graph.New()

	nodes := []*graph.Node{
		{ID: "repoA/main.go::main", Kind: graph.KindFunction, Name: "main", FilePath: "repoA/main.go", Language: "go", RepoPrefix: "repoA", StartLine: 1, EndLine: 10},
		{ID: "repoA/pkg/util.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoA/pkg/util.go", Language: "go", RepoPrefix: "repoA", StartLine: 1, EndLine: 5},
		{ID: "repoB/lib.go::Helper", Kind: graph.KindFunction, Name: "Helper", FilePath: "repoB/lib.go", Language: "go", RepoPrefix: "repoB", StartLine: 1, EndLine: 8},
		{ID: "repoB/lib.go::Process", Kind: graph.KindFunction, Name: "Process", FilePath: "repoB/lib.go", Language: "go", RepoPrefix: "repoB", StartLine: 10, EndLine: 20},
	}
	for _, n := range nodes {
		g.AddNode(n)
	}

	edges := []*graph.Edge{
		{From: "repoA/main.go::main", To: "repoA/pkg/util.go::Helper", Kind: graph.EdgeCalls, FilePath: "repoA/main.go", Line: 5},
		{From: "repoA/main.go::main", To: "repoB/lib.go::Process", Kind: graph.EdgeCalls, FilePath: "repoA/main.go", Line: 7, CrossRepo: true},
		{From: "repoB/lib.go::Process", To: "repoB/lib.go::Helper", Kind: graph.EdgeCalls, FilePath: "repoB/lib.go", Line: 12},
	}
	for _, e := range edges {
		g.AddEdge(e)
	}

	return g
}

func TestSearchSymbolsInRepo(t *testing.T) {
	g := buildMultiRepoTestGraph()
	e := NewEngine(g)

	// Search for "Helper" scoped to repoA — should only return repoA's Helper.
	results := e.SearchSymbolsInRepo("Helper", "repoA", 10)
	require.Len(t, results, 1)
	assert.Equal(t, "repoA/pkg/util.go::Helper", results[0].ID)

	// Search for "Helper" scoped to repoB — should only return repoB's Helper.
	results = e.SearchSymbolsInRepo("Helper", "repoB", 10)
	require.Len(t, results, 1)
	assert.Equal(t, "repoB/lib.go::Helper", results[0].ID)

	// Search for "Process" scoped to repoA — should return nothing.
	results = e.SearchSymbolsInRepo("Process", "repoA", 10)
	assert.Empty(t, results)

	// Search for a non-existent repo — should return nothing.
	results = e.SearchSymbolsInRepo("Helper", "repoC", 10)
	assert.Empty(t, results)
}

func TestSearchSymbolsInRepo_Limit(t *testing.T) {
	g := buildMultiRepoTestGraph()
	e := NewEngine(g)

	// With limit=1, should return at most 1 result.
	results := e.SearchSymbolsInRepo("Helper", "repoA", 1)
	assert.Len(t, results, 1)
}

func TestGetFileSymbolsInRepo(t *testing.T) {
	g := buildMultiRepoTestGraph()
	e := NewEngine(g)

	// Get symbols for repoB/lib.go scoped to repoB.
	sg := e.GetFileSymbolsInRepo("repoB/lib.go", "repoB")
	assert.Len(t, sg.Nodes, 2) // Helper and Process
	ids := nodeIDs(sg.Nodes)
	assert.Contains(t, ids, "repoB/lib.go::Helper")
	assert.Contains(t, ids, "repoB/lib.go::Process")

	// Get symbols for repoB/lib.go scoped to repoA — should return nothing.
	sg = e.GetFileSymbolsInRepo("repoB/lib.go", "repoA")
	assert.Empty(t, sg.Nodes)

	// Get symbols for a non-existent file — should return empty.
	sg = e.GetFileSymbolsInRepo("nonexistent.go", "repoA")
	assert.Empty(t, sg.Nodes)
}

func TestGetFileSymbolsInRepo_Edges(t *testing.T) {
	g := buildMultiRepoTestGraph()
	e := NewEngine(g)

	// Edges should be included when at least one endpoint is in the filtered set.
	sg := e.GetFileSymbolsInRepo("repoB/lib.go", "repoB")
	assert.Greater(t, len(sg.Edges), 0)
}

func nodeIDs(nodes []*graph.Node) []string {
	ids := make([]string, len(nodes))
	for i, n := range nodes {
		ids[i] = n.ID
	}
	return ids
}
