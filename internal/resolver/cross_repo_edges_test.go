package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/zzet/gortex/internal/graph"
)

type crossRepoBatchSpy struct {
	graph.Store
	addEdgeCalls  int
	addBatchCalls int
}

func (s *crossRepoBatchSpy) AddEdge(edge *graph.Edge) {
	s.addEdgeCalls++
	s.Store.AddEdge(edge)
}

func (s *crossRepoBatchSpy) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatchCalls++
	s.Store.AddBatch(nodes, edges)
}

// countOutEdgesByKind returns how many out-edges of the given kind the
// node fromID has.
func countOutEdgesByKind(g graph.Store, fromID string, kind graph.EdgeKind) int {
	n := 0
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// firstOutEdgeByKind returns the first out-edge of fromID with the given
// kind, or nil.
func firstOutEdgeByKind(g graph.Store, fromID string, kind graph.EdgeKind) *graph.Edge {
	for _, e := range g.GetOutEdges(fromID) {
		if e.Kind == kind {
			return e
		}
	}
	return nil
}

func TestDetectCrossRepoEdges_CallsAcrossRepos(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Callee", Kind: graph.KindFunction, Name: "Callee", FilePath: "repoB/b.go", RepoPrefix: "repoB"})

	base := &graph.Edge{
		From: "repoA/a.go::Caller", To: "repoB/b.go::Callee",
		Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 7,
		Confidence: 0.8, Origin: graph.OriginASTInferred,
	}
	g.AddEdge(base)

	emitted := DetectCrossRepoEdges(g)
	assert.Equal(t, 1, emitted)

	// Parallel edge materialised.
	cr := firstOutEdgeByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls)
	if assert.NotNil(t, cr, "expected a cross_repo_calls edge") {
		assert.Equal(t, "repoB/b.go::Callee", cr.To)
		assert.Equal(t, "repoA/a.go", cr.FilePath)
		assert.Equal(t, 7, cr.Line)
		// Origin / confidence inherited from the base edge.
		assert.Equal(t, graph.OriginASTInferred, cr.Origin)
		assert.Equal(t, 0.8, cr.Confidence)
		assert.True(t, cr.CrossRepo)
		assert.Equal(t, "calls", cr.Meta["base_kind"])
		assert.Equal(t, "repoA", cr.Meta["source_repo"])
		assert.Equal(t, "repoB", cr.Meta["target_repo"])
	}

	// Base edge keeps its kind and now carries the bool flag.
	assert.Equal(t, graph.EdgeCalls, base.Kind)
	assert.True(t, base.CrossRepo)
}

func TestDetectCrossRepoEdges_ImplementsAndExtends(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Impl", Kind: graph.KindType, Name: "Impl", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Iface", Kind: graph.KindInterface, Name: "Iface", FilePath: "repoB/b.go", RepoPrefix: "repoB"})
	g.AddNode(&graph.Node{ID: "repoA/a.go::Child", Kind: graph.KindType, Name: "Child", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Parent", Kind: graph.KindType, Name: "Parent", FilePath: "repoB/b.go", RepoPrefix: "repoB"})

	g.AddEdge(&graph.Edge{From: "repoA/a.go::Impl", To: "repoB/b.go::Iface", Kind: graph.EdgeImplements, FilePath: "repoA/a.go", Line: 3, Origin: graph.OriginASTResolved})
	g.AddEdge(&graph.Edge{From: "repoA/a.go::Child", To: "repoB/b.go::Parent", Kind: graph.EdgeExtends, FilePath: "repoA/a.go", Line: 4, Origin: graph.OriginASTResolved})

	emitted := DetectCrossRepoEdges(g)
	assert.Equal(t, 2, emitted)

	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Impl", graph.EdgeCrossRepoImplements))
	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Child", graph.EdgeCrossRepoExtends))
}

func TestDetectCrossRepoEdges_SameRepoUntouched(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoA/b.go::Callee", Kind: graph.KindFunction, Name: "Callee", FilePath: "repoA/b.go", RepoPrefix: "repoA"})

	base := &graph.Edge{From: "repoA/a.go::Caller", To: "repoA/b.go::Callee", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 1}
	g.AddEdge(base)

	emitted := DetectCrossRepoEdges(g)
	assert.Equal(t, 0, emitted)
	assert.Equal(t, 0, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls))
	assert.False(t, base.CrossRepo)
}

func TestDetectCrossRepoEdges_SkipsStubsAndUnstampedNodes(t *testing.T) {
	g := graph.New()
	// Caller in repoA; one edge to an unresolved stub, one to a node
	// whose RepoPrefix was never stamped.
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "loose.go::Loose", Kind: graph.KindFunction, Name: "Loose", FilePath: "loose.go"}) // no RepoPrefix

	g.AddEdge(&graph.Edge{From: "repoA/a.go::Caller", To: "unresolved::Missing", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 1})
	g.AddEdge(&graph.Edge{From: "repoA/a.go::Caller", To: "external::net/http", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 2})
	g.AddEdge(&graph.Edge{From: "repoA/a.go::Caller", To: "loose.go::Loose", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 3})

	emitted := DetectCrossRepoEdges(g)
	assert.Equal(t, 0, emitted)
	assert.Equal(t, 0, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls))
}

func TestDetectCrossRepoEdges_Idempotent(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Callee", Kind: graph.KindFunction, Name: "Callee", FilePath: "repoB/b.go", RepoPrefix: "repoB"})
	g.AddEdge(&graph.Edge{From: "repoA/a.go::Caller", To: "repoB/b.go::Callee", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 7})

	first := DetectCrossRepoEdges(g)
	second := DetectCrossRepoEdges(g)
	assert.Equal(t, 1, first)
	// Re-running re-counts the same relationship but graph.AddEdge
	// dedupes by edgeKey — the parallel edge is not duplicated.
	assert.Equal(t, 1, second)
	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls))
}

// The pass must not feed on its own output: a cross_repo_* edge is not
// itself a base kind, so a second pass finds nothing new from it.
func TestDetectCrossRepoEdges_DoesNotRecurseOnOwnOutput(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoB/b.go::Callee", Kind: graph.KindFunction, Name: "Callee", FilePath: "repoB/b.go", RepoPrefix: "repoB"})
	g.AddEdge(&graph.Edge{From: "repoA/a.go::Caller", To: "repoB/b.go::Callee", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 7})

	DetectCrossRepoEdges(g)
	// Exactly one base + one parallel edge — never a cross_repo_calls
	// parallel of a cross_repo_calls edge.
	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCalls))
	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls))
}

func TestDetectCrossRepoEdgesForReposUsesIncidentScopeAndBatchWrite(t *testing.T) {
	base := graph.New()
	for _, node := range []*graph.Node{
		{ID: "repoA/a.go::A", Kind: graph.KindFunction, FilePath: "repoA/a.go", RepoPrefix: "repoA"},
		{ID: "repoB/b.go::B", Kind: graph.KindFunction, FilePath: "repoB/b.go", RepoPrefix: "repoB"},
		{ID: "repoC/c.go::C", Kind: graph.KindFunction, FilePath: "repoC/c.go", RepoPrefix: "repoC"},
		{ID: "repoD/d.go::D", Kind: graph.KindFunction, FilePath: "repoD/d.go", RepoPrefix: "repoD"},
	} {
		base.AddNode(node)
	}
	inbound := &graph.Edge{From: "repoA/a.go::A", To: "repoB/b.go::B", Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 3}
	outside := &graph.Edge{From: "repoC/c.go::C", To: "repoD/d.go::D", Kind: graph.EdgeCalls, FilePath: "repoC/c.go", Line: 4}
	base.AddBatch(nil, []*graph.Edge{inbound, outside})
	spy := &crossRepoBatchSpy{Store: base}

	emitted := DetectCrossRepoEdgesForRepos(spy, []string{"repoB"})
	assert.Equal(t, 1, emitted, "an inbound relationship must be included in the changed-target frontier")
	assert.True(t, inbound.CrossRepo)
	assert.False(t, outside.CrossRepo)
	assert.Equal(t, 0, spy.addEdgeCalls)
	assert.Equal(t, 1, spy.addBatchCalls)
	assert.Equal(t, 1, countOutEdgesByKind(base, inbound.From, graph.EdgeCrossRepoCalls))
	assert.Equal(t, 0, countOutEdgesByKind(base, outside.From, graph.EdgeCrossRepoCalls))

	// The exact batched existence lookup makes a warm replay write-free.
	DetectCrossRepoEdgesForRepos(spy, []string{"repoB"})
	assert.Equal(t, 1, spy.addBatchCalls)
}

// candidateQuerySpy records whether the candidate layer was queried at
// all. The whole-graph query joins BOTH endpoints of every base-kind edge
// against the node table, so "returned nothing" and "was never asked" cost
// wildly different amounts — on a single 117k-node repository the query ran
// 745 seconds to return zero rows.
type candidateQuerySpy struct {
	graph.Store
	queries int
}

func (s *candidateQuerySpy) CrossRepoCandidates(baseKinds []graph.EdgeKind) []graph.CrossRepoCandidateRow {
	s.queries++
	if inner, ok := s.Store.(graph.CrossRepoCandidates); ok {
		return inner.CrossRepoCandidates(baseKinds)
	}
	return crossRepoCandidatesFallback(s.Store, baseKinds, nil, nil)
}

func (s *candidateQuerySpy) CrossRepoCandidatesForRepos(baseKinds []graph.EdgeKind, repoPrefixes []string) []graph.CrossRepoCandidateRow {
	s.queries++
	if inner, ok := s.Store.(graph.ScopedCrossRepoCandidates); ok {
		return inner.CrossRepoCandidatesForRepos(baseKinds, repoPrefixes)
	}
	return crossRepoCandidatesFallback(s.Store, baseKinds, stringSet(repoPrefixes), nil)
}

func singleRepoGraphWithBaseEdges(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()
	g.AddNode(&graph.Node{ID: "repoA/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repoA/a.go", RepoPrefix: "repoA"})
	g.AddNode(&graph.Node{ID: "repoA/b.go::Callee", Kind: graph.KindFunction, Name: "Callee", FilePath: "repoA/b.go", RepoPrefix: "repoA"})
	g.AddEdge(&graph.Edge{
		From: "repoA/a.go::Caller", To: "repoA/b.go::Callee",
		Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 3,
	})
	return g
}

// A one-repository graph cannot hold a cross-repo edge, so the pass must
// prove that from the prefix set rather than by running the join. This is
// not a micro-optimisation: the workspace derivation that follows an
// untrack down to one repo reaches this pass every time.
func TestDetectCrossRepoEdges_SkipsQueryOnSingleRepoGraph(t *testing.T) {
	spy := &candidateQuerySpy{Store: singleRepoGraphWithBaseEdges(t)}

	assert.Zero(t, DetectCrossRepoEdges(spy))
	assert.Zero(t, spy.queries,
		"a single-repo graph must not query the cross-repo candidate layer at all")
}

func TestDetectCrossRepoEdgesForRepos_SkipsQueryOnSingleRepoGraph(t *testing.T) {
	spy := &candidateQuerySpy{Store: singleRepoGraphWithBaseEdges(t)}

	assert.Zero(t, DetectCrossRepoEdgesForRepos(spy, []string{"repoA"}))
	assert.Zero(t, spy.queries)
}

// The guard is a precondition, not a mute button: a second repository must
// bring the query straight back.
func TestDetectCrossRepoEdges_QueriesOnceASecondRepoExists(t *testing.T) {
	g := singleRepoGraphWithBaseEdges(t)
	g.AddNode(&graph.Node{ID: "repoB/c.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "repoB/c.go", RepoPrefix: "repoB"})
	g.AddEdge(&graph.Edge{
		From: "repoA/a.go::Caller", To: "repoB/c.go::Other",
		Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 4,
	})
	spy := &candidateQuerySpy{Store: g}

	assert.Equal(t, 1, DetectCrossRepoEdges(spy))
	assert.Equal(t, 1, spy.queries)
	assert.Equal(t, 1, countOutEdgesByKind(g, "repoA/a.go::Caller", graph.EdgeCrossRepoCalls))
}

// A repo prefix of "" is the solo-repo/synthetic marker, never a second
// repository — two of them must stay unqueryable.
func TestDetectCrossRepoEdges_EmptyPrefixIsNotASecondRepo(t *testing.T) {
	g := singleRepoGraphWithBaseEdges(t)
	g.AddNode(&graph.Node{ID: "dep::example.com/x", Kind: graph.KindFunction, Name: "x", FilePath: ""})
	g.AddEdge(&graph.Edge{
		From: "repoA/a.go::Caller", To: "dep::example.com/x",
		Kind: graph.EdgeCalls, FilePath: "repoA/a.go", Line: 5,
	})
	spy := &candidateQuerySpy{Store: g}

	assert.Zero(t, DetectCrossRepoEdges(spy))
	assert.Zero(t, spy.queries)
}
