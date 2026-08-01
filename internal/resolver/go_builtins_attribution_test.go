package resolver

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type builtinBatchCountingStore struct {
	graph.Store
	getNodesByIDs int
	addNode       int
	addBatch      int
}

func (s *builtinBatchCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesByIDs++
	return s.Store.GetNodesByIDs(ids)
}

func (s *builtinBatchCountingStore) AddNode(n *graph.Node) {
	s.addNode++
	s.Store.AddNode(n)
}

func (s *builtinBatchCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatch++
	s.Store.AddBatch(nodes, edges)
}

func TestAttributeGoBuiltins_FunctionCall(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::Run"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "Run", FilePath: "pkg/foo.go", Language: "go"})
	edge := &graph.Edge{From: owner, To: "unresolved::append", Kind: graph.EdgeArgOf, FilePath: "pkg/foo.go", Line: 5}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "builtin::go::append", edge.To,
		"call to `append` must retarget onto builtin::go::append")
	n := g.GetNode("builtin::go::append")
	require.NotNil(t, n, "KindBuiltin node must be materialised")
	assert.Equal(t, graph.KindBuiltin, n.Kind)
	assert.Equal(t, "append", n.Name)
	assert.Equal(t, "go", n.Language)
	assert.Equal(t, true, n.Meta["builtin"])
	assert.Equal(t, "func", n.Meta["builtin_kind"])
}

func TestAttributeGoBuiltins_Type(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::Handler"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "Handler", FilePath: "pkg/foo.go", Language: "go"})

	paramID := owner + "#param:s"
	g.AddNode(&graph.Node{ID: paramID, Kind: graph.KindParam, Name: "s", FilePath: "pkg/foo.go", Language: "go"})
	edge := &graph.Edge{From: paramID, To: "unresolved::string", Kind: graph.EdgeTypedAs, FilePath: "pkg/foo.go", Line: 1}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "builtin::go::type::string", edge.To,
		"typed_as `string` must retarget onto builtin::go::type::string")
	n := g.GetNode("builtin::go::type::string")
	require.NotNil(t, n)
	assert.Equal(t, graph.KindBuiltin, n.Kind)
	assert.Equal(t, "type", n.Meta["builtin_kind"])
}

func TestAttributeGoBuiltinsUsesEnclosingOwnerRepo(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::Handler"
	g.AddNode(&graph.Node{
		ID: owner, Kind: graph.KindFunction, Name: "Handler", FilePath: "pkg/foo.go",
		Language: "go", RepoPrefix: "repo", WorkspaceID: "workspace", ProjectID: "project",
	})
	edge := &graph.Edge{From: owner + "#param:synthetic", To: "unresolved::len", Kind: graph.EdgeArgOf, Line: 1}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "repo::builtin::go::len", edge.To)
	n := g.GetNode(edge.To)
	require.NotNil(t, n)
	assert.Equal(t, "repo", n.RepoPrefix, "per-repo builtin node must participate in purge/scoping")
	assert.Equal(t, "workspace", n.WorkspaceID, "builtin must inherit the source workspace boundary")
	assert.Equal(t, "project", n.ProjectID, "builtin must inherit the source project boundary")
}

func TestAttributeGoBuiltins_DedupedAcrossManyEdges(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::F"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "F", FilePath: "pkg/foo.go", Language: "go"})

	// Many calls to len from the same function.
	for i := 1; i <= 5; i++ {
		g.AddEdge(&graph.Edge{From: owner, To: "unresolved::len", Kind: graph.EdgeArgOf, FilePath: "pkg/foo.go", Line: i})
	}

	New(g).attributeGoBuiltins()

	// Exactly one KindBuiltin node should be created regardless of
	// how many edges referenced it.
	count := 0
	for n := range g.NodesByKind(graph.KindBuiltin) {
		if n.ID == "builtin::go::len" {
			count++
		}
	}
	assert.Equal(t, 1, count, "exactly one KindBuiltin per unique builtin")
}

func TestAttributeGoBuiltinsBatchesSourceReadsAndNodeWrites(t *testing.T) {
	base := graph.New()
	for i := 0; i < 100; i++ {
		owner := fmt.Sprintf("opaque::caller-%03d", i)
		base.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "caller", Language: "go", RepoPrefix: "repo"})
		base.AddEdge(&graph.Edge{From: owner, To: "unresolved::len", Kind: graph.EdgeCalls, Line: i + 1})
	}
	store := &builtinBatchCountingStore{Store: base}

	New(store).attributeGoBuiltins()

	assert.Equal(t, 1, store.getNodesByIDs, "builtin sources must be loaded in one set query")
	assert.Equal(t, 0, store.addNode, "builtin materialisation must not write one node at a time")
	assert.Equal(t, 1, store.addBatch, "builtin nodes must be materialised in one batch")
	for i := 0; i < 100; i++ {
		owner := fmt.Sprintf("opaque::caller-%03d", i)
		assert.True(t, hasEdgeKind(base, owner, "repo::builtin::go::len", graph.EdgeCalls))
	}
}

func TestAttributeGoBuiltins_NonGoLeftAlone(t *testing.T) {
	g := graph.New()
	// A Python source emitting a reference to `len` (Python builtin)
	// — must NOT get attributed to Go's `builtin::go::len`.
	owner := "pkg/app.py::process"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "process", FilePath: "pkg/app.py", Language: "python"})
	edge := &graph.Edge{From: owner, To: "unresolved::len", Kind: graph.EdgeArgOf, FilePath: "pkg/app.py", Line: 1}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "unresolved::len", edge.To,
		"Python source must NOT cross-bind to Go's len builtin")
}

func TestAttributeGoBuiltins_UnknownNameLeftAlone(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::F"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "F", FilePath: "pkg/foo.go", Language: "go"})
	edge := &graph.Edge{From: owner, To: "unresolved::myCustomFunc", Kind: graph.EdgeArgOf, FilePath: "pkg/foo.go", Line: 1}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "unresolved::myCustomFunc", edge.To,
		"non-builtin names must stay unresolved")
}

func TestAttributeGoBuiltins_QualifiedShapeLeftAlone(t *testing.T) {
	g := graph.New()
	owner := "pkg/foo.go::F"
	g.AddNode(&graph.Node{ID: owner, Kind: graph.KindFunction, Name: "F", FilePath: "pkg/foo.go", Language: "go"})

	// `*.len` is qualified — leave to other passes.
	edge := &graph.Edge{From: owner, To: "unresolved::*.len", Kind: graph.EdgeArgOf, FilePath: "pkg/foo.go", Line: 1}
	g.AddEdge(edge)

	New(g).attributeGoBuiltins()

	assert.Equal(t, "unresolved::*.len", edge.To, "qualified `*.len` shape must be left alone")
}
