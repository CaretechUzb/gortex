package resolver

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/frameworkgate"
	"github.com/zzet/gortex/internal/graph"
)

func odooOnly() frameworkgate.Set { return frameworkgate.New([]string{"odoo"}) }

// recordingStore captures what actually reached the backend.
type recordingStore struct {
	graph.Store
	edges      []*graph.Edge
	nodes      []*graph.Node
	reindexed  []*graph.Edge
	provenance []*graph.Edge
	removed    []string
}

func (s *recordingStore) AddEdge(e *graph.Edge) { s.edges = append(s.edges, e) }

func (s *recordingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.nodes = append(s.nodes, nodes...)
	s.edges = append(s.edges, edges...)
}

func edgeFrom(from string) *graph.Edge {
	return &graph.Edge{From: from, To: "odoo/target.py::Target", Kind: graph.EdgeCalls}
}

func TestFrameworkRepoPrefix(t *testing.T) {
	cases := map[string]string{
		"odoo/addons/sale/models/x.py::Cls.method": "odoo",
		"local@aurora/a/b.js::fn":                  "local@aurora",
		// Synthetic IDs precede any repository and must not be
		// attributed to one — "unresolved" is not a repo prefix.
		"unresolved::odoo::xmlid::sale/order": "",
		"gortex::builtin::go::make":           "",
		"noslash":                             "",
		"":                                    "",
	}
	for id, want := range cases {
		assert.Equal(t, want, frameworkRepoPrefix(id), "id %q", id)
	}
}

// The unconfigured workspace must not pay for the gate at all.
func TestNewFrameworkRepoGate_NilWhenNothingNarrows(t *testing.T) {
	assert.Nil(t, newFrameworkRepoGate(nil))
	assert.Nil(t, newFrameworkRepoGate(map[string]frameworkgate.Set{
		"gortex": {},
		"odoo":   {},
	}))
	assert.NotNil(t, newFrameworkRepoGate(map[string]frameworkgate.Set{
		"gortex": {},
		"odoo":   odooOnly(),
	}))
}

func TestFrameworkRepoGate_AdmitsPerRepo(t *testing.T) {
	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{
		"odoo":   odooOnly(),
		"gortex": {}, // no list — allows everything
	})
	require.NotNil(t, gate)

	assert.True(t, gate.admits("odoo/a.py::X", "odoo"), "odoo repo keeps the pass it allowed")
	assert.False(t, gate.admits("odoo/a.py::X", "value-ref"), "odoo repo excluded this pass")
	assert.True(t, gate.admits("gortex/a.go::X", "value-ref"), "unconfigured repo keeps everything")

	// Fail open: an unknown or unattributable source must never lose its
	// edge to a config it never declared.
	assert.True(t, gate.admits("untracked/a.py::X", "value-ref"))
	assert.True(t, gate.admits("unresolved::odoo::xmlid::sale/order", "value-ref"))
}

func TestFrameworkRepoGate_ExcludesPass(t *testing.T) {
	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{
		"odoo":   odooOnly(),
		"gortex": {},
	})
	assert.True(t, gate.excludesPass("value-ref"), "some repo excludes it, so the store must be wrapped")
	assert.False(t, gate.excludesPass("odoo"), "every repo admits it, so no wrapper is needed")
	assert.False(t, (*frameworkRepoGate)(nil).excludesPass("value-ref"))
}

// A pass no repository excludes must get the bare store back — the
// wrapper is an interface hop on every edge and has to stay off the
// unconfigured path.
func TestNewFrameworkRepoGateStore_UnwrappedWhenNothingExcluded(t *testing.T) {
	base := &recordingStore{}
	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{"odoo": odooOnly()})

	assert.Same(t, base, newFrameworkRepoGateStore(base, gate, "odoo", nil))
	assert.Same(t, base, newFrameworkRepoGateStore(base, nil, "value-ref", nil))
	assert.NotSame(t, base, newFrameworkRepoGateStore(base, gate, "value-ref", nil))
}

func TestFrameworkRepoGateStore_AddEdgeDropsExcludedRepo(t *testing.T) {
	base := &recordingStore{}
	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{
		"odoo":   odooOnly(),
		"gortex": {},
	})
	store := newFrameworkRepoGateStore(base, gate, "value-ref", nil)

	store.AddEdge(edgeFrom("odoo/a.py::X"))   // excluded
	store.AddEdge(edgeFrom("gortex/a.go::X")) // allowed
	store.AddEdge(edgeFrom("odoo/b.py::Y"))   // excluded

	require.Len(t, base.edges, 1)
	assert.Equal(t, "gortex/a.go::X", base.edges[0].From)
	assert.Equal(t, 2, droppedFrameworkRepoEdges(store))
}

// The batch path is the one the synthesizers actually take: legacy
// AddEdge loops are staged and flushed as a single AddBatch.
func TestFrameworkRepoGateStore_AddBatchFiltersEdgesKeepsNodes(t *testing.T) {
	base := &recordingStore{}
	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{
		"odoo":   odooOnly(),
		"gortex": {},
	})
	store := newFrameworkRepoGateStore(base, gate, "react-resolve", nil)

	node := &graph.Node{ID: "odoo/a.py::X", RepoPrefix: "odoo"}
	store.AddBatch([]*graph.Node{node}, []*graph.Edge{
		edgeFrom("odoo/a.py::X"),
		edgeFrom("gortex/a.go::X"),
	})

	require.Len(t, base.edges, 1)
	assert.Equal(t, "gortex/a.go::X", base.edges[0].From)
	assert.Len(t, base.nodes, 1, "nodes are descriptions, not assertions into a repo")
	assert.Equal(t, 1, droppedFrameworkRepoEdges(store))
}

// The gate must not corrupt the caller's slice: AddBatch filters in
// place over a fresh backing array, never over the input.
func TestFrameworkRepoGateStore_AddBatchLeavesCallerSliceIntact(t *testing.T) {
	base := &recordingStore{}
	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{"odoo": odooOnly()})
	store := newFrameworkRepoGateStore(base, gate, "value-ref", nil)

	edges := []*graph.Edge{edgeFrom("odoo/a.py::X"), edgeFrom("gortex/a.go::X")}
	store.AddBatch(nil, edges)

	require.Len(t, edges, 2, "caller's slice keeps its length")
	assert.Equal(t, "odoo/a.py::X", edges[0].From, "caller's slice keeps its contents")
	assert.Equal(t, "gortex/a.go::X", edges[1].From)
}

func TestDroppedFrameworkRepoEdges_UnwrappedStoreDroppedNothing(t *testing.T) {
	assert.Equal(t, 0, droppedFrameworkRepoEdges(&recordingStore{}))
	assert.Equal(t, 0, droppedFrameworkRepoEdges(nil))
}

func TestWithFrameworkAllowByRepo(t *testing.T) {
	byRepo := map[string]frameworkgate.Set{"odoo": odooOnly()}
	o := resolveFrameworkSynthOptions([]FrameworkSynthOption{
		WithFrameworkAllowByRepo(byRepo),
	})
	require.Len(t, o.allowedByRepo, 1)
	assert.False(t, o.allowedByRepo["odoo"].Allows("value-ref"))

	// The two options are independent: the union still governs execution.
	o = resolveFrameworkSynthOptions([]FrameworkSynthOption{
		WithAllowedFrameworks(frameworkgate.Set{}),
		WithFrameworkAllowByRepo(byRepo),
	})
	assert.True(t, o.allowed.Allows("value-ref"), "union unset: the pass still runs")
	assert.False(t, o.allowedByRepo["odoo"].Allows("value-ref"), "but odoo refuses its edges")
}

// The wiring that matters: legacy synthesizers call AddEdge in a loop,
// which the batch layer stages and flushes as ONE AddBatch. The gate sits
// under that batch store, so this proves a refused edge never reaches the
// backend even though the pass thought it wrote one.
func TestFrameworkRepoGate_BatchFlushHonoursGate(t *testing.T) {
	base := &recordingStore{}
	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{
		"odoo":   odooOnly(),
		"gortex": {},
	})
	gated := newFrameworkRepoGateStore(base, gate, "value-ref", nil)

	n := runLegacyFrameworkSynthWithCache(gated, nil, func(store graph.Store) int {
		store.AddEdge(edgeFrom("odoo/a.py::X"))
		store.AddEdge(edgeFrom("gortex/a.go::X"))
		return 2 // what the pass believes it wrote
	})

	require.Equal(t, 2, n, "the pass still reports what it staged")
	require.Len(t, base.edges, 1, "only the admitting repository's edge is committed")
	assert.Equal(t, "gortex/a.go::X", base.edges[0].From)
	assert.Equal(t, 1, droppedFrameworkRepoEdges(gated),
		"the drop is counted so the report can subtract it from Edges")
}

// A pass every repository allows must reach the backend untouched, with
// no wrapper in the path at all.
func TestFrameworkRepoGate_AllowedPassFlushesUngated(t *testing.T) {
	base := &recordingStore{}
	gate := newFrameworkRepoGate(map[string]frameworkgate.Set{
		"odoo":   odooOnly(),
		"gortex": {},
	})
	gated := newFrameworkRepoGateStore(base, gate, "odoo", nil)
	require.Same(t, base, gated)

	runLegacyFrameworkSynthWithCache(gated, nil, func(store graph.Store) int {
		store.AddEdge(edgeFrom("odoo/a.py::X"))
		store.AddEdge(edgeFrom("gortex/a.go::X"))
		return 2
	})

	assert.Len(t, base.edges, 2, "a universally allowed pass keeps every edge")
	assert.Equal(t, 0, droppedFrameworkRepoEdges(gated))
}

// Resolver-shaped passes rewrite an existing unresolved edge instead of
// adding one. Gating only AddEdge left every one of them ungated in
// production, so each mutation path needs its own coverage.
func (s *recordingStore) ReindexEdge(e *graph.Edge, oldTo string) {
	s.reindexed = append(s.reindexed, e)
}

func (s *recordingStore) ReindexEdges(batch []graph.EdgeReindex) {
	for _, r := range batch {
		s.reindexed = append(s.reindexed, r.Edge)
	}
}

func (s *recordingStore) SetEdgeProvenance(e *graph.Edge, origin string) bool {
	s.provenance = append(s.provenance, e)
	return true
}

func (s *recordingStore) SetEdgeProvenanceBatch(batch []graph.EdgeProvenanceUpdate) int {
	for _, u := range batch {
		s.provenance = append(s.provenance, u.Edge)
	}
	return len(batch)
}

func (s *recordingStore) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	s.removed = append(s.removed, from)
	return true
}

func gateFor(base graph.Store, pass string) graph.Store {
	return newFrameworkRepoGateStore(base, newFrameworkRepoGate(map[string]frameworkgate.Set{
		"odoo":   odooOnly(),
		"gortex": {},
	}), pass, nil)
}

func TestFrameworkRepoGateStore_ReindexEdgeRefusesExcludedRepo(t *testing.T) {
	base := &recordingStore{}
	store := gateFor(base, "value-ref")

	store.ReindexEdge(edgeFrom("odoo/a.py::X"), "unresolved::x")
	store.ReindexEdge(edgeFrom("gortex/a.go::X"), "unresolved::x")

	require.Len(t, base.reindexed, 1, "the excluded repo's edge must stay unresolved")
	assert.Equal(t, "gortex/a.go::X", base.reindexed[0].From)
	assert.Equal(t, 1, droppedFrameworkRepoEdges(store))
}

func TestFrameworkRepoGateStore_ReindexEdgesFiltersBatch(t *testing.T) {
	base := &recordingStore{}
	store := gateFor(base, "react-resolve")

	store.ReindexEdges([]graph.EdgeReindex{
		{Edge: edgeFrom("odoo/a.py::X"), OldTo: "unresolved::x"},
		{Edge: edgeFrom("gortex/a.go::X"), OldTo: "unresolved::x"},
	})

	require.Len(t, base.reindexed, 1)
	assert.Equal(t, "gortex/a.go::X", base.reindexed[0].From)
	assert.Equal(t, 1, droppedFrameworkRepoEdges(store))
}

// A reindex may move the edge's identity. Neither endpoint may be an
// excluding repository: not the row's current owner, nor its destination.
func TestFrameworkRepoGateStore_ReindexChecksBothEndsOfAMovingIdentity(t *testing.T) {
	base := &recordingStore{}
	store := gateFor(base, "value-ref")

	// Moving a row OUT of the excluded repo is still touching that repo.
	store.ReindexEdges([]graph.EdgeReindex{
		{Edge: edgeFrom("gortex/a.go::X"), OldFrom: "odoo/a.py::X"},
	})
	assert.Empty(t, base.reindexed, "oldFrom in an excluded repo must be refused")

	// Moving a row INTO the excluded repo is refused on the new From.
	store.ReindexEdges([]graph.EdgeReindex{
		{Edge: edgeFrom("odoo/a.py::X"), OldFrom: "gortex/a.go::X"},
	})
	assert.Empty(t, base.reindexed, "a move into an excluded repo must be refused")
	assert.Equal(t, 2, droppedFrameworkRepoEdges(store))
}

func TestFrameworkRepoGateStore_ProvenanceAndRemoveAreGated(t *testing.T) {
	base := &recordingStore{}
	store := gateFor(base, "value-ref")

	assert.False(t, store.SetEdgeProvenance(edgeFrom("odoo/a.py::X"), "heuristic"))
	assert.True(t, store.SetEdgeProvenance(edgeFrom("gortex/a.go::X"), "heuristic"))
	store.SetEdgeProvenanceBatch([]graph.EdgeProvenanceUpdate{
		{Edge: edgeFrom("odoo/b.py::Y"), NewOrigin: "heuristic"},
		{Edge: edgeFrom("gortex/b.go::Y"), NewOrigin: "heuristic"},
	})
	require.Len(t, base.provenance, 2)

	assert.False(t, store.RemoveEdge("odoo/a.py::X", "t", graph.EdgeCalls),
		"an excluded pass may not delete that repository's edges either")
	assert.True(t, store.RemoveEdge("gortex/a.go::X", "t", graph.EdgeCalls))
	assert.Equal(t, []string{"gortex/a.go::X"}, base.removed)
}

// A pass every repository allows keeps the bare store, so none of the
// mutation overrides are even in the path.
func TestFrameworkRepoGateStore_AllowedPassMutatesUngated(t *testing.T) {
	base := &recordingStore{}
	store := gateFor(base, "odoo")
	require.Same(t, base, store)

	store.ReindexEdge(edgeFrom("odoo/a.py::X"), "unresolved::x")
	assert.Len(t, base.reindexed, 1)
}

// Claiming resolvers are gated on candidate admission rather than through
// a gated store, so their refusals have to be counted separately or the
// report's repo_gated silently covers synthesizers alone.
func TestRunClaimingResolvers_CountsRepoGatedRefusals(t *testing.T) {
	g := graph.New()
	o := resolveFrameworkSynthOptions([]FrameworkSynthOption{
		WithFrameworkAllowByRepo(map[string]frameworkgate.Set{"odoo": odooOnly()}),
	})

	claimed, gated, _ := runClaimingResolversScopedCounted(g, nil, o)
	assert.NotNil(t, claimed)
	assert.GreaterOrEqual(t, gated, 0, "the counter must be plumbed, not dropped")

	// The exported wrapper keeps its original single-value shape.
	assert.NotNil(t, RunClaimingResolversScoped(g, nil))
}

// repoGated counts REFUSED WRITES, so an edge the resolver would never
// have claimed must not score. Counting the repo gate before Claims made
// every pending edge in a narrowing repository score against every pass
// that repository excluded.
func TestRunClaimingResolvers_UnclaimedEdgesAreNotCountedAsRefusals(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{
		ID: "odoo/a.py::Q", Kind: graph.KindType, Name: "Q", FilePath: "odoo/a.py", Language: "python",
	})
	// An unresolved edge in the excluded repository that no claiming
	// resolver recognises.
	g.AddEdge(&graph.Edge{
		From: "odoo/a.py::Q", To: graph.UnresolvedMarker + "not_a_descriptor_name",
		Kind: graph.EdgeReferences,
	})

	o := resolveFrameworkSynthOptions([]FrameworkSynthOption{
		WithFrameworkAllowByRepo(map[string]frameworkgate.Set{"odoo": frameworkgate.New([]string{"flask"})}),
	})
	_, gated, _ := runClaimingResolversScopedCounted(g, nil, o)
	assert.Zero(t, gated, "an edge no resolver claims is not a refused write")
}
