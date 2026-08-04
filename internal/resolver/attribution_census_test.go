package resolver

import (
	"fmt"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type attributionCensusRecordingStore struct {
	graph.Store

	lightEdges     []*graph.Edge
	lightCalls     int
	lightKinds     []graph.EdgeKind
	callableCalls  int
	finderCalls    [][]graph.EdgeIdentity
	current        map[graph.EdgeIdentity]*graph.Edge
	reindexBatches [][]graph.EdgeReindex
}

func (s *attributionCensusRecordingStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.lightCalls++
	s.lightKinds = append([]graph.EdgeKind(nil), kinds...)
	wanted := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		wanted[kind] = struct{}{}
	}
	return func(yield func(*graph.Edge) bool) {
		for _, edge := range s.lightEdges {
			if edge == nil {
				continue
			}
			if _, ok := wanted[edge.Kind]; !ok {
				continue
			}
			if !yield(edge) {
				return
			}
		}
	}
}

func (s *attributionCensusRecordingStore) CallableBindingNodesSeq(
	_ ...graph.NodeKind,
) iter.Seq[graph.CallableBindingNode] {
	s.callableCalls++
	return func(func(graph.CallableBindingNode) bool) {}
}

func (s *attributionCensusRecordingStore) FindEdgesByIdentities(
	identities []graph.EdgeIdentity,
) map[graph.EdgeIdentity]*graph.Edge {
	s.finderCalls = append(s.finderCalls, append([]graph.EdgeIdentity(nil), identities...))
	found := make(map[graph.EdgeIdentity]*graph.Edge, len(identities))
	for _, identity := range identities {
		if edge := s.current[identity]; edge != nil {
			found[identity] = edge
		}
	}
	return found
}

func (s *attributionCensusRecordingStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexBatches = append(s.reindexBatches, append([]graph.EdgeReindex(nil), batch...))
}

type attributionCensusNoProjectionStore struct {
	graph.Store
}

func TestBuildPostBareAttributionCensusFallsBackWithoutProjectionCapabilities(t *testing.T) {
	store := &attributionCensusNoProjectionStore{Store: graph.New()}
	census, stats := (&Resolver{graph: store}).buildPostBareAttributionCensus(defaultAttributionCensusLimits)

	assert.Nil(t, census)
	assert.Equal(t, "missing_projection_capability", stats.fallbackReason)
	assert.Zero(t, stats.rowsScanned)
}

func TestBuildPostBareAttributionCensusUsesOneLightWalk(t *testing.T) {
	resolvedMethod := "repo/pkg/file.go::Worker.Run"
	edges := []*graph.Edge{
		{From: "caller", To: resolvedMethod, Kind: graph.EdgeCalls, FilePath: "pkg/file.go", Line: 9},
		{From: "arg-1", To: "unresolved::Do", Kind: graph.EdgeArgOf, FilePath: "pkg/file.go", Line: 10},
		{From: "arg-2", To: "repo::unresolved::*.Run", Kind: graph.EdgeValueFlow, FilePath: "pkg/file.go", Line: 9},
		{From: "fn#local:x", To: "unresolved::T", Kind: graph.EdgeReferences, FilePath: "pkg/file.go", Line: 11},
		{From: "fn#local:y", To: "unresolved::pkg.T", Kind: graph.EdgeTypedAs, FilePath: "pkg/file.go", Line: 12},
		{From: "fn#local:z", To: "repo::unresolved::T", Kind: graph.EdgeReturns, FilePath: "pkg/file.go", Line: 13},
		{From: "ignored", To: "unresolved::Do", Kind: graph.EdgeMemberOf},
	}
	store := &attributionCensusRecordingStore{Store: graph.New(), lightEdges: edges}
	r := &Resolver{graph: store}

	census, stats := r.buildPostBareAttributionCensus(defaultAttributionCensusLimits)
	require.NotNil(t, census)
	defer census.close()

	assert.Equal(t, 1, store.lightCalls)
	assert.Equal(t, 1, store.callableCalls)
	assert.Equal(t, postBareAttributionCensusKinds, store.lightKinds)
	assert.Equal(t, 6, stats.rowsScanned)
	assert.Equal(t, 2, stats.dataflowCandidates)
	assert.Equal(t, 1, stats.genericCandidates)
	assert.Empty(t, stats.fallbackReason)
	assert.Equal(t, resolvedMethod, uniqueSiteCallee(census.callee.bySite[siteKey("pkg/file.go", 9)], "Run"))

	wantBytes := attributionCensusIdentityBytes(graph.EdgeIdentityFor(edges[1])) +
		attributionCensusIdentityBytes(graph.EdgeIdentityFor(edges[2])) +
		attributionCensusIdentityBytes(graph.EdgeIdentityFor(edges[3]))
	assert.Equal(t, wantBytes, stats.retainedBytes)

	census.releaseDataflow()
	assert.Nil(t, census.callee)
	assert.Nil(t, census.dataflowCandidates)
	assert.Len(t, census.genericCandidates, 1)
}

func TestBuildPostBareAttributionCensusFallsBackAtCaps(t *testing.T) {
	edges := []*graph.Edge{
		{From: "arg-1", To: "unresolved::First", Kind: graph.EdgeArgOf, FilePath: "pkg/file.go"},
		{From: "arg-2", To: "unresolved::Second", Kind: graph.EdgeArgOf, FilePath: "pkg/file.go"},
	}

	t.Run("candidate_count", func(t *testing.T) {
		store := &attributionCensusRecordingStore{Store: graph.New(), lightEdges: edges}
		census, stats := (&Resolver{graph: store}).buildPostBareAttributionCensus(attributionCensusLimits{
			maxCandidates:    1,
			maxRetainedBytes: 1 << 20,
		})
		assert.Nil(t, census)
		assert.Equal(t, "candidate_cap", stats.fallbackReason)
		assert.Equal(t, 2, stats.rowsScanned)
		assert.Equal(t, 1, stats.dataflowCandidates)
		assert.Zero(t, store.callableCalls)
	})

	t.Run("retained_bytes", func(t *testing.T) {
		store := &attributionCensusRecordingStore{Store: graph.New(), lightEdges: edges}
		firstBytes := attributionCensusIdentityBytes(graph.EdgeIdentityFor(edges[0]))
		census, stats := (&Resolver{graph: store}).buildPostBareAttributionCensus(attributionCensusLimits{
			maxCandidates:    10,
			maxRetainedBytes: firstBytes - 1,
		})
		assert.Nil(t, census)
		assert.Equal(t, "retained_byte_cap", stats.fallbackReason)
		assert.Equal(t, 1, stats.rowsScanned)
		assert.Zero(t, stats.dataflowCandidates)
		assert.Zero(t, stats.retainedBytes)
		assert.Zero(t, store.callableCalls)
	})
}

func TestReindexAttributionIdentitiesBatchedBoundsHydration(t *testing.T) {
	const count = attributionReindexBatchSize*2 + 7
	store := &attributionCensusRecordingStore{
		Store:   graph.New(),
		current: make(map[graph.EdgeIdentity]*graph.Edge, count),
	}
	identities := make([]graph.EdgeIdentity, 0, count)
	for i := 0; i < count; i++ {
		edge := &graph.Edge{
			From: fmt.Sprintf("from-%d", i), To: fmt.Sprintf("unresolved::target-%d", i),
			Kind: graph.EdgeReferences, FilePath: "pkg/file.go", Line: i + 1,
		}
		identity := graph.EdgeIdentityFor(edge)
		identities = append(identities, identity)
		store.current[identity] = edge
	}
	r := &Resolver{graph: store}

	r.reindexAttributionIdentitiesBatched(store, identities, func(edge *graph.Edge) string {
		oldTo := edge.To
		edge.To = "resolved::" + edge.From
		return oldTo
	})

	require.Len(t, store.finderCalls, 3)
	for _, call := range store.finderCalls {
		assert.LessOrEqual(t, len(call), attributionReindexBatchSize)
	}
	assertAttributionReindexBatches(t, store.reindexBatches, count)
}

func TestReindexAttributionIdentitiesBatchedDropsStaleIdentity(t *testing.T) {
	old := graph.EdgeIdentity{From: "from", To: "unresolved::Old", Kind: graph.EdgeReferences, FilePath: "pkg/file.go", Line: 1}
	currentEdge := &graph.Edge{From: old.From, To: "resolved::New", Kind: old.Kind, FilePath: old.FilePath, Line: old.Line}
	store := &attributionCensusRecordingStore{
		Store: graph.New(),
		current: map[graph.EdgeIdentity]*graph.Edge{
			graph.EdgeIdentityFor(currentEdge): currentEdge,
		},
	}
	rewrites := 0

	(&Resolver{graph: store}).reindexAttributionIdentitiesBatched(
		store,
		[]graph.EdgeIdentity{old},
		func(edge *graph.Edge) string {
			rewrites++
			return edge.To
		},
	)

	assert.Zero(t, rewrites)
	assert.Empty(t, store.reindexBatches)
	assert.Equal(t, "resolved::New", currentEdge.To)
}

func TestReindexAttributionIdentitiesBatchedUsesCurrentSameKeyPayload(t *testing.T) {
	identity := graph.EdgeIdentity{
		From: "from", To: "unresolved::Target", Kind: graph.EdgeReferences,
		FilePath: "pkg/file.go", Line: 1,
	}
	replacement := &graph.Edge{
		From: identity.From, To: identity.To, Kind: identity.Kind,
		FilePath: identity.FilePath, Line: identity.Line,
		Origin: graph.OriginLSPResolved,
		Meta:   map[string]any{"payload": "fresh"},
	}
	store := &attributionCensusRecordingStore{
		Store:   graph.New(),
		current: map[graph.EdgeIdentity]*graph.Edge{identity: replacement},
	}

	(&Resolver{graph: store}).reindexAttributionIdentitiesBatched(
		store,
		[]graph.EdgeIdentity{identity},
		func(edge *graph.Edge) string {
			oldTo := edge.To
			edge.To = "resolved::Target"
			return oldTo
		},
	)

	require.Len(t, store.reindexBatches, 1)
	require.Len(t, store.reindexBatches[0], 1)
	reindexed := store.reindexBatches[0][0]
	assert.Same(t, replacement, reindexed.Edge)
	assert.Equal(t, identity.To, reindexed.OldTo)
	assert.Equal(t, graph.OriginLSPResolved, reindexed.Edge.Origin)
	assert.Equal(t, "fresh", reindexed.Edge.Meta["payload"])
}

func TestPostBareAttributionCensusMatchesLegacyPasses(t *testing.T) {
	legacyGraph, sources := buildPostBareAttributionParityGraph()
	sharedGraph, _ := buildPostBareAttributionParityGraph()

	legacy := &Resolver{graph: legacyGraph}
	legacy.bindDataflowCalleeRefs()
	legacy.bindGenericParamRefs()

	shared := &Resolver{graph: sharedGraph}
	census, stats := shared.buildPostBareAttributionCensus(defaultAttributionCensusLimits)
	require.NotNil(t, census)
	require.Empty(t, stats.fallbackReason)
	shared.bindDataflowCalleeRefsFromCensus(census)
	census.releaseDataflow()
	shared.bindGenericParamRefsFromCensus(census)
	census.releaseGeneric()
	census.close()

	for source, want := range sources {
		assert.Equal(t, want, soleOutTarget(t, legacyGraph, source), "legacy source %s", source)
		assert.Equal(t, want, soleOutTarget(t, sharedGraph, source), "shared source %s", source)
	}
}

func TestPostBareAttributionCensusCapFallbackUsesLegacyPasses(t *testing.T) {
	g, sources := buildPostBareAttributionParityGraph()
	previousLimits := defaultAttributionCensusLimits
	defaultAttributionCensusLimits = attributionCensusLimits{
		maxCandidates:    1,
		maxRetainedBytes: 1 << 20,
	}
	defer func() { defaultAttributionCensusLimits = previousLimits }()

	New(g).runFileAttributionPassesLocked()

	for source, want := range sources {
		assert.Equal(t, want, soleOutTarget(t, g, source), "fallback source %s", source)
	}
}

func buildPostBareAttributionParityGraph() (*graph.Graph, map[string]string) {
	g := graph.New()
	file := "pkg/file.go"
	owner := file + "::Owner"
	functionTarget := file + "::Do"
	methodTarget := file + "::Worker.Run"
	tparamTarget := owner + "#tparam:T"
	argFunction := owner + "#arg:0"
	argMethod := owner + "#arg:1"
	genericUse := owner + "#local:value"

	nodes := []*graph.Node{
		{ID: owner, Kind: graph.KindFunction, Name: "Owner", FilePath: file, Language: "go"},
		{ID: functionTarget, Kind: graph.KindFunction, Name: "Do", FilePath: file, Language: "go"},
		{ID: methodTarget, Kind: graph.KindMethod, Name: "Run", FilePath: file, Language: "go"},
		{ID: tparamTarget, Kind: graph.KindGenericParam, Name: "T", FilePath: file, Language: "go"},
		{ID: argFunction, Kind: graph.KindVariable, Name: "arg0", FilePath: file, Language: "go"},
		{ID: argMethod, Kind: graph.KindVariable, Name: "arg1", FilePath: file, Language: "go"},
		{ID: genericUse, Kind: graph.KindVariable, Name: "value", FilePath: file, Language: "go"},
	}
	edges := []*graph.Edge{
		{From: owner, To: methodTarget, Kind: graph.EdgeCalls, FilePath: file, Line: 20},
		{From: argFunction, To: "unresolved::Do", Kind: graph.EdgeArgOf, FilePath: file, Line: 10},
		{From: argMethod, To: "unresolved::*.Run", Kind: graph.EdgeValueFlow, FilePath: file, Line: 20},
		{From: genericUse, To: "unresolved::T", Kind: graph.EdgeTypedAs, FilePath: file, Line: 30},
	}
	g.AddBatch(nodes, edges)
	return g, map[string]string{
		argFunction: functionTarget,
		argMethod:   methodTarget,
		genericUse:  tparamTarget,
	}
}

func soleOutTarget(t *testing.T, g *graph.Graph, source string) string {
	t.Helper()
	edges := g.GetOutEdges(source)
	require.Len(t, edges, 1)
	return edges[0].To
}
