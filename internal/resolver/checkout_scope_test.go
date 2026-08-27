package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Tracking a git worktree of an already-tracked repository gives the
// workspace two prefixes over one body of code. Every binder below used
// to see a second, byte-identical definition of every symbol and bind to
// it — and because "local-bench/" sorts before "local/", the WRONG
// checkout won essentially every time.

// siblingGraph builds a graph whose two repo prefixes are checkouts of
// one repository, with the sibling prefix sorting first.
func siblingGraph(t *testing.T) *graph.Graph {
	t.Helper()
	g := graph.New()
	g.SetCheckoutGroups(map[string]string{
		"local":       "/src/local",
		"local-bench": "/src/local",
	})
	return g
}

// The headline case: a model reference in `local` must bind to `local`'s
// own declaring class, never to the identical class in the worktree.
func TestOdooModelBinding_IgnoresSiblingCheckout(t *testing.T) {
	g := siblingGraph(t)
	odooModelClass(g, "local/order.py::SaleOrder", "sale.order")
	odooModelClass(g, "local-bench/order.py::SaleOrder", "sale.order")
	odooModelStub(g, "local/w.py::Wizard", "sale.order", graph.EdgeExtends)

	ResolveOdooRefs(g)

	assert.NotNil(t, odooFindEdge(g, graph.EdgeExtends, "local/w.py::Wizard", "local/order.py::SaleOrder"),
		"the model must bind inside the asking checkout")
	assert.Nil(t, odooFindEdge(g, graph.EdgeExtends, "local/w.py::Wizard", "local-bench/order.py::SaleOrder"),
		"a sibling checkout holds the same class, not another one")
}

// Same fixture, no grouping published: both bind. This is what the
// grouping actually changes — and the reason an unwired store must keep
// its old behaviour rather than silently losing the fan-out.
func TestOdooModelBinding_WithoutGroupingKeepsCrossRepoFanOut(t *testing.T) {
	g := graph.New()
	odooModelClass(g, "local/order.py::SaleOrder", "sale.order")
	odooModelClass(g, "local-bench/order.py::SaleOrder", "sale.order")
	odooModelStub(g, "local/w.py::Wizard", "sale.order", graph.EdgeExtends)

	ResolveOdooRefs(g)

	assert.NotNil(t, odooFindEdge(g, graph.EdgeExtends, "local/w.py::Wizard", "local-bench/order.py::SaleOrder"),
		"two independent repositories must still fan out")
}

// Genuine cross-repository Odoo layouts — a custom addon inheriting a
// model only core declares — must keep resolving.
func TestOdooModelBinding_KeepsGenuineCrossRepoTarget(t *testing.T) {
	g := siblingGraph(t)
	odooModelClass(g, "core/order.py::SaleOrder", "sale.order")
	odooModelStub(g, "local/w.py::Wizard", "sale.order", graph.EdgeExtends)

	ResolveOdooRefs(g)

	assert.NotNil(t, odooFindEdge(g, graph.EdgeExtends, "local/w.py::Wizard", "core/order.py::SaleOrder"),
		"a repository that is not a sibling checkout is still a valid target")
}

// External IDs resolve to exactly one record, previously the
// lexicographically lowest across the whole graph. The asking record's
// own repository has to win.
func TestOdooXMLIDBinding_PrefersOwnCheckout(t *testing.T) {
	g := siblingGraph(t)
	for _, id := range []string{"local/views.xml::sale.view_order", "local-bench/views.xml::sale.view_order"} {
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindResource, Name: "sale.view_order",
			Language: "odoo_xml", Meta: map[string]any{"odoo_xml_id": "sale.view_order"},
		})
	}
	g.AddNode(&graph.Node{
		ID: "local/views.xml::sale.view_order_inherit", Kind: graph.KindResource,
		Name: "sale.view_order_inherit", Language: "odoo_xml",
		Meta: map[string]any{"odoo_xml_id": "sale.view_order_inherit"},
	})
	odooStub(g, "local/views.xml::sale.view_order_inherit",
		odooXMLIDStubPrefix+"sale.view_order", graph.EdgeExtends, odooXMLVia,
		map[string]any{"odoo_xml_id": "sale.view_order"})

	ResolveOdooRefs(g)

	assert.NotNil(t, odooFindEdge(g, graph.EdgeExtends,
		"local/views.xml::sale.view_order_inherit", "local/views.xml::sale.view_order"),
		"inherit_id must bind inside the asking checkout")
	assert.Nil(t, odooFindEdge(g, graph.EdgeExtends,
		"local/views.xml::sale.view_order_inherit", "local-bench/views.xml::sale.view_order"),
		"the worktree's copy of the record is the same record")
}

// odooIndex is the shared lookup behind every single-target Odoo binder,
// so its ordering rule is worth pinning directly.
func TestOdooIndexLookup_Ordering(t *testing.T) {
	g := siblingGraph(t)
	ix := odooIndex{}
	ix.put("k", "local-bench/a.py::X")
	ix.put("k", "local/b.py::X")
	ix.put("k", "core/c.py::X")

	assert.Equal(t, "local/b.py::X", ix.lookup(g, "local/caller.py::C", "k"),
		"the asking repository wins outright")
	assert.Equal(t, "local-bench/a.py::X", ix.lookup(g, "local-bench/caller.py::C", "k"),
		"the rule is symmetric — each checkout resolves inside itself")
	assert.Equal(t, "core/c.py::X", ix.lookup(g, "addons/caller.py::C", "k"),
		"an unrelated repository falls back to the lowest ID")
	assert.Equal(t, "", ix.lookup(g, "local/caller.py::C", "missing"))

	// The case the sibling rule exists for: the asking checkout declares
	// nothing, so the fallback runs — and must step over the duplicate in
	// its own worktree rather than take it because it sorts first.
	sib := odooIndex{}
	sib.put("k", "local/a.py::X")
	sib.put("k", "core/z.py::X")
	assert.Equal(t, "core/z.py::X", sib.lookup(g, "local-bench/caller.py::C", "k"),
		"a sibling checkout is never the fallback target")
}

// The cross_repo_* parallel-edge layer is the backstop: whatever produced
// a base edge across two checkouts, it must not be promoted to a
// cross-repository relationship.
func TestDetectCrossRepoEdges_SkipsSiblingCheckouts(t *testing.T) {
	g := siblingGraph(t)
	g.AddNode(&graph.Node{ID: "local/a.py::A", Kind: graph.KindType, RepoPrefix: "local", FilePath: "local/a.py"})
	g.AddNode(&graph.Node{ID: "local-bench/b.py::B", Kind: graph.KindType, RepoPrefix: "local-bench", FilePath: "local-bench/b.py"})
	e := &graph.Edge{From: "local/a.py::A", To: "local-bench/b.py::B", Kind: graph.EdgeExtends, FilePath: "local/a.py", Line: 3}
	g.AddEdge(e)

	require.Equal(t, 0, DetectCrossRepoEdges(g),
		"an edge between two checkouts of one repository crosses no boundary")

	crKind, ok := graph.CrossRepoKindFor(graph.EdgeExtends)
	require.True(t, ok)
	assert.Nil(t, odooFindEdge(g, crKind, "local/a.py::A", "local-bench/b.py::B"),
		"no parallel cross_repo_* edge may be minted")
	assert.False(t, e.CrossRepo, "the base edge must not be flagged cross-repo")
}

// Same fixture with the two prefixes belonging to genuinely different
// repositories: the layer still works.
func TestDetectCrossRepoEdges_KeepsGenuineCrossRepoEdge(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "local/a.py::A", Kind: graph.KindType, RepoPrefix: "local", FilePath: "local/a.py"})
	g.AddNode(&graph.Node{ID: "core/b.py::B", Kind: graph.KindType, RepoPrefix: "core", FilePath: "core/b.py"})
	e := &graph.Edge{From: "local/a.py::A", To: "core/b.py::B", Kind: graph.EdgeExtends, FilePath: "local/a.py", Line: 3}
	g.AddEdge(e)

	require.Equal(t, 1, DetectCrossRepoEdges(g))
	assert.True(t, e.CrossRepo)
}

// The whole two-way candidate join is proportional to the base-kind edge
// set whether or not a row can survive, so a workspace that is one
// repository checked out twice must not run it at all.
func TestCrossRepoPossible_CountsRepositoriesNotPrefixes(t *testing.T) {
	g := siblingGraph(t)
	g.AddNode(&graph.Node{ID: "local/a.py::A", Kind: graph.KindType, RepoPrefix: "local", FilePath: "local/a.py"})
	g.AddNode(&graph.Node{ID: "local-bench/a.py::A", Kind: graph.KindType, RepoPrefix: "local-bench", FilePath: "local-bench/a.py"})

	assert.False(t, crossRepoPossible(g),
		"two prefixes over one repository cannot hold a cross-repo edge")

	g.AddNode(&graph.Node{ID: "core/b.py::B", Kind: graph.KindType, RepoPrefix: "core", FilePath: "core/b.py"})
	assert.True(t, crossRepoPossible(g),
		"a third, independent repository makes cross-repo edges possible again")
}

// The import-reachability gate is the cross-repo resolver's evidence
// rule: a name-only candidate in repo R is eligible only when the caller's
// file imports R. A sibling checkout can clear that bar — the two trees
// are identical, so a mis-bound import in either direction makes the
// worktree "reachable" — and must still be refused.
func TestReachabilityChecker_RefusesSiblingCheckout(t *testing.T) {
	g := siblingGraph(t)
	g.AddNode(&graph.Node{
		ID: "local/a.py::A", Kind: graph.KindFunction,
		RepoPrefix: "local", FilePath: "local/a.py",
	})
	e := &graph.Edge{From: "local/a.py::A", To: "unresolved::B", Kind: graph.EdgeCalls, FilePath: "local/a.py"}

	cr := NewCrossRepo(g)
	cr.reachableReposByFile = map[string]map[string]struct{}{
		"local/a.py": {"local-bench": {}, "core": {}},
	}
	reachable := cr.reachabilityChecker(e)

	assert.False(t, reachable("local-bench"),
		"a separate checkout of the caller's own repository is never a target")
	assert.True(t, reachable("core"), "a genuinely imported repository stays reachable")
	assert.True(t, reachable("local"), "the caller's own repository is always reachable")
	assert.True(t, reachable(""), "repo-independent targets stay reachable")
}

func TestWithoutSiblingCheckouts(t *testing.T) {
	g := siblingGraph(t)
	candidates := []*graph.Node{
		{ID: "local-bench/a.py::X", RepoPrefix: "local-bench"},
		{ID: "local/a.py::X", RepoPrefix: "local"},
		{ID: "core/a.py::X", RepoPrefix: "core"},
	}

	cr := NewCrossRepo(g)
	kept := cr.withoutSiblingCheckouts("local", candidates)
	require.Len(t, kept, 2)
	for _, n := range kept {
		assert.NotEqual(t, "local-bench", n.RepoPrefix)
	}

	// No grouping published: the slice comes back untouched, allocation
	// included, so the common workspace pays nothing.
	plain := NewCrossRepo(graph.New())
	assert.Equal(t, candidates, plain.withoutSiblingCheckouts("local", candidates))
}

// The framework write gate is the general backstop: it covers EVERY
// registered synthesizer and claiming resolver, not just the Odoo family,
// on every edge-write path they use. A pass nobody has narrowed still
// gets wrapped once a worktree is tracked, because the checkout rule is
// an invariant rather than a permission.
func TestFrameworkGateStore_RefusesSiblingCheckoutEdges(t *testing.T) {
	src := siblingGraph(t)
	base := graph.New()
	store := newFrameworkRepoGateStore(base, nil, "some-pass", src)
	require.NotSame(t, graph.Store(base), store,
		"a tracked worktree must wrap the store even with no allow-list")

	store.AddEdge(&graph.Edge{From: "local/a.py::A", To: "local-bench/b.py::B", Kind: graph.EdgeReferences})
	store.AddEdge(&graph.Edge{From: "local/a.py::A", To: "local/b.py::B", Kind: graph.EdgeReferences})
	store.AddEdge(&graph.Edge{From: "local/a.py::A", To: "core/b.py::B", Kind: graph.EdgeReferences})
	store.AddEdge(&graph.Edge{From: "local/a.py::A", To: "unresolved::B", Kind: graph.EdgeReferences})

	assert.Equal(t, 1, droppedSiblingCheckoutEdges(store), "only the cross-checkout edge is refused")
	assert.Equal(t, 0, droppedFrameworkRepoEdges(store), "no allow-list refused anything")
	assert.Nil(t, odooFindEdge(base, graph.EdgeReferences, "local/a.py::A", "local-bench/b.py::B"))
	assert.NotNil(t, odooFindEdge(base, graph.EdgeReferences, "local/a.py::A", "local/b.py::B"))
	assert.NotNil(t, odooFindEdge(base, graph.EdgeReferences, "local/a.py::A", "core/b.py::B"))
	assert.NotNil(t, odooFindEdge(base, graph.EdgeReferences, "local/a.py::A", "unresolved::B"))
}

// The batch and rebind paths matter as much as AddEdge: the claiming
// resolvers never call AddEdge at all, they mutate an existing edge's To
// and ask for it to be persisted.
func TestFrameworkGateStore_RefusesSiblingCheckoutOnEveryWritePath(t *testing.T) {
	src := siblingGraph(t)
	base := graph.New()
	store := newFrameworkRepoGateStore(base, nil, "some-pass", src)

	sibling := func() *graph.Edge {
		return &graph.Edge{From: "local/a.py::A", To: "local-bench/b.py::B", Kind: graph.EdgeCalls}
	}
	store.AddBatch(nil, []*graph.Edge{sibling()})
	store.ReindexEdge(sibling(), "unresolved::B")
	store.ReindexEdges([]graph.EdgeReindex{{Edge: sibling(), OldTo: "unresolved::B"}})
	store.SetEdgeProvenance(sibling(), graph.OriginASTInferred)
	store.SetEdgeProvenanceBatch([]graph.EdgeProvenanceUpdate{{Edge: sibling(), NewOrigin: graph.OriginASTInferred}})

	assert.Equal(t, 5, droppedSiblingCheckoutEdges(store),
		"every write path a framework pass can reach must refuse it")
}

// Without a tracked worktree the wrapper is not installed at all, so the
// unconfigured workspace keeps its bare store and pays no interface hop.
func TestFrameworkGateStore_NotInstalledWithoutWorktrees(t *testing.T) {
	base := graph.New()
	assert.Same(t, graph.Store(base), newFrameworkRepoGateStore(base, nil, "some-pass", graph.New()))
}
