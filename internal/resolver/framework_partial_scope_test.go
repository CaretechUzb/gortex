package resolver

import (
	"fmt"
	"iter"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type frameworkTailCountingStore struct {
	graph.Store
	getNode, getNodesByIDs                int
	findByName, findByNames               int
	getInEdges, getInEdgesByIDs           int
	addEdge, addBatch, reindexEdges       int
	removeEdge, removeEdgesExact          int
	repoEdgesByKinds, repoNodeIDsByKind   int
	allNodesLight, repoNodesLight         int
	repoPrefixes, unresolvedIdentityScans int
	frameworkCensusScans                  int
}

func (s *frameworkTailCountingStore) GetNode(id string) *graph.Node {
	s.getNode++
	return s.Store.GetNode(id)
}

func (s *frameworkTailCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesByIDs++
	return s.Store.GetNodesByIDs(ids)
}

func (s *frameworkTailCountingStore) FindNodesByName(name string) []*graph.Node {
	s.findByName++
	return s.Store.FindNodesByName(name)
}

func (s *frameworkTailCountingStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	s.findByNames++
	return s.Store.FindNodesByNames(names)
}

func (s *frameworkTailCountingStore) GetInEdges(id string) []*graph.Edge {
	s.getInEdges++
	return s.Store.GetInEdges(id)
}

func (s *frameworkTailCountingStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getInEdgesByIDs++
	return s.Store.GetInEdgesByNodeIDs(ids)
}

func (s *frameworkTailCountingStore) AddEdge(edge *graph.Edge) {
	s.addEdge++
	s.Store.AddEdge(edge)
}

func (s *frameworkTailCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatch++
	s.Store.AddBatch(nodes, edges)
}

func (s *frameworkTailCountingStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexEdges++
	s.Store.ReindexEdges(batch)
}

func (s *frameworkTailCountingStore) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	s.removeEdge++
	return s.Store.RemoveEdge(from, to, kind)
}

func (s *frameworkTailCountingStore) RemoveEdgesExact(edges []*graph.Edge) int {
	s.removeEdgesExact++
	return s.Store.(graph.ExactEdgeBatchRemover).RemoveEdgesExact(edges)
}

func (s *frameworkTailCountingStore) RepoEdgesByKinds(prefixes []string, kinds []graph.EdgeKind) []graph.RepoEdgeRow {
	s.repoEdgesByKinds++
	return s.Store.(graph.RepoEdgeKindReader).RepoEdgesByKinds(prefixes, kinds)
}

func (s *frameworkTailCountingStore) RepoNodeIDsByKinds(prefixes []string, kinds []graph.NodeKind) []string {
	s.repoNodeIDsByKind++
	return s.Store.(graph.RepoNodeKindIDReader).RepoNodeIDsByKinds(prefixes, kinds)
}

func (s *frameworkTailCountingStore) AllNodesLight() []*graph.Node {
	s.allNodesLight++
	return s.Store.(graph.NodeLightScanner).AllNodesLight()
}

func (s *frameworkTailCountingStore) RepoNodesLight(prefixes []string) []*graph.Node {
	s.repoNodesLight++
	return s.Store.(graph.RepoLightNodeReader).RepoNodesLight(prefixes)
}

func (s *frameworkTailCountingStore) RepoPrefixes() []string {
	s.repoPrefixes++
	return s.Store.RepoPrefixes()
}

func (s *frameworkTailCountingStore) ScanUnresolvedEdgeIdentitiesBatched(
	kinds []graph.EdgeKind,
	batchSize int,
	yield func([]graph.EdgeIdentity) bool,
) {
	s.unresolvedIdentityScans++
	s.Store.(graph.UnresolvedEdgeIdentityBatchScanner).ScanUnresolvedEdgeIdentitiesBatched(kinds, batchSize, yield)
}

func (s *frameworkTailCountingStore) FrameworkCensusEdgesSeq(
	kinds ...graph.EdgeKind,
) iter.Seq[graph.FrameworkCensusEdge] {
	s.frameworkCensusScans++
	return graph.FrameworkCensusEdgesSeq(s.Store, kinds...)
}

func addDjangoRepo(g *graph.Graph, repo string, count int) {
	iterID := repo + "::models.py::ModelIterable.__iter__"
	g.AddNode(&graph.Node{ID: iterID, Kind: graph.KindMethod, Name: "__iter__", FilePath: repo + "/models.py", Language: "python", RepoPrefix: repo,
		Meta: map[string]any{"receiver": "ModelIterable"}})
	classID := repo + "::query.py::QuerySet"
	g.AddNode(&graph.Node{ID: classID, Kind: graph.KindType, Name: "QuerySet", FilePath: repo + "/query.py", Language: "python", RepoPrefix: repo,
		Meta: map[string]any{"django_iterable_class": "ModelIterable"}})
	for i := 0; i < count; i++ {
		methodID := fmt.Sprintf("%s::query.py::QuerySet.iterator%d", repo, i)
		g.AddNode(&graph.Node{ID: methodID, Kind: graph.KindMethod, Name: "iterator", FilePath: repo + "/query.py", Language: "python", RepoPrefix: repo,
			Meta: map[string]any{"receiver": "QuerySet"}})
		target := "unresolved::*._iterable_class"
		if i%2 != 0 {
			target = repo + "::" + target
		}
		g.AddEdge(&graph.Edge{From: methodID, To: target, Kind: graph.EdgeCalls, FilePath: repo + "/query.py", Line: i + 1})
	}
}

func TestRunClaimingResolversScopedBatchesAndBoundsRepoFrontier(t *testing.T) {
	g := graph.New()
	addDjangoRepo(g, "changed", 40)
	addDjangoRepo(g, "untouched", 40)
	counting := &frameworkTailCountingStore{Store: g}

	claimed := RunClaimingResolversScoped(counting, map[string]bool{"changed": true})
	require.Equal(t, 40, claimed[SynthDjangoDescriptor])
	require.Zero(t, counting.getNode, "no per-edge node lookup")
	require.Zero(t, counting.findByName, "no per-edge name lookup")
	require.Equal(t, 2, counting.findByNames, "one admission probe + one batch bind")
	require.Equal(t, 1, counting.reindexEdges)
	require.Equal(t, 1, counting.getInEdgesByIDs, "one indexed reverse-vocabulary lookup")
	require.Zero(t, counting.repoEdgesByKinds, "scoped claiming must not scan repository edge kinds")

	untouched := 0
	for edge := range g.EdgesByKind(graph.EdgeCalls) {
		if edge != nil && graph.IsUnresolvedTarget(edge.To) && edge.From[:len("untouched")] == "untouched" {
			untouched++
		}
	}
	require.Equal(t, 40, untouched, "partial claiming must not touch another repository")
}

func TestRunClaimingResolversUsesExactVocabularyForFullScope(t *testing.T) {
	g := graph.New()
	addDjangoRepo(g, "alpha", 40)
	addDjangoRepo(g, "beta", 40)
	g.AddEdge(&graph.Edge{
		From: "alpha::query.py::QuerySet.iterator0", To: "alpha::" + graph.UnresolvedMarker + "*._iterable_class",
		Kind: graph.EdgeReferences, FilePath: "alpha/query.py", Line: 100,
	})
	g.AddEdge(&graph.Edge{
		From: "alpha::query.py::QuerySet.iterator0", To: "alpha::" + graph.UnresolvedMarker + "*._iterable_class",
		Kind: graph.EdgeExtends, FilePath: "alpha/query.py", Line: 101,
	})
	g.AddEdge(&graph.Edge{
		From: "alpha::query.py::QuerySet.iterator0", To: "legacy-only::" + graph.UnresolvedMarker + "*._iterable_class",
		Kind: graph.EdgeReferences, FilePath: "alpha/query.py", Line: 102,
	})
	counting := &frameworkTailCountingStore{Store: g}

	claimed := RunClaimingResolvers(counting)
	require.Equal(t, 82, claimed[SynthDjangoDescriptor], "calls and references are candidates; unrelated kinds are not")
	require.Equal(t, 1, counting.unresolvedIdentityScans, "full claiming should use one bounded identity scan")
	require.Zero(t, counting.repoPrefixes, "edge-only legacy prefixes must not depend on the node-backed prefix census")
	require.Zero(t, counting.getInEdgesByIDs, "full claiming should not synthesize target IDs from node prefixes")
	require.Zero(t, counting.repoEdgesByKinds, "full claiming must not decode all calls and references")
}

func TestScopedClaimingCandidatesFallsBackForUnboundedResolver(t *testing.T) {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "alpha::caller", Kind: graph.KindMethod, RepoPrefix: "alpha"})
	g.AddEdge(&graph.Edge{From: "alpha::caller", To: graph.UnresolvedMarker + "widget", Kind: graph.EdgeCalls})
	g.AddEdge(&graph.Edge{From: "alpha::caller", To: "alpha::" + graph.UnresolvedMarker + "other", Kind: graph.EdgeReferences})

	pending := scopedClaimingCandidates(g, nil, []ClaimingResolver{fakeClaimingResolver{target: "alpha::resolved"}})
	require.Len(t, pending, 2, "an unbounded resolver must retain the complete-scan fallback")
}

func TestFrameworkCandidateSummaryScopedUsesOnlyRepoLightProjection(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "changed::Handler", Kind: graph.KindMethod, Name: "Handle", Language: "csharp", RepoPrefix: "changed"},
		{ID: "untouched::Task", Kind: graph.KindMethod, Name: "perform", Language: "ruby", RepoPrefix: "untouched"},
	}, nil)
	counting := &frameworkTailCountingStore{Store: g}
	summary := summarizeFrameworkCandidates(counting, map[string]bool{"changed": true})
	require.Equal(t, 1, summary.all["dotnet"])
	require.Zero(t, summary.all["ruby"])
	require.Equal(t, 1, counting.repoNodesLight)
	require.Zero(t, counting.allNodesLight, "partial census must not materialize the workspace")
}

func addCSharpDispatchRepo(g *graph.Graph, repo string) (caller, ifaceMethod, implMethod string) {
	iface := repo + "::I"
	ifaceMethod = repo + "::I.Do"
	impl := repo + "::Impl"
	implMethod = repo + "::Impl.Do"
	caller = repo + "::Caller.Run"
	g.AddBatch([]*graph.Node{
		{ID: iface, Kind: graph.KindInterface, Name: "I", Language: "csharp", RepoPrefix: repo},
		{ID: ifaceMethod, Kind: graph.KindMethod, Name: "Do", Language: "csharp", RepoPrefix: repo, Meta: map[string]any{"receiver": "I", "iface_member": true}},
		{ID: impl, Kind: graph.KindType, Name: "Impl", Language: "csharp", RepoPrefix: repo},
		{ID: implMethod, Kind: graph.KindMethod, Name: "Do", Language: "csharp", RepoPrefix: repo, Meta: map[string]any{"receiver": "Impl"}},
		{ID: caller, Kind: graph.KindMethod, Name: "Run", Language: "csharp", RepoPrefix: repo, Meta: map[string]any{"receiver": "Caller"}},
	}, []*graph.Edge{
		{From: ifaceMethod, To: iface, Kind: graph.EdgeMemberOf},
		{From: implMethod, To: impl, Kind: graph.EdgeMemberOf},
		{From: impl, To: iface, Kind: graph.EdgeImplements},
		{From: caller, To: ifaceMethod, Kind: graph.EdgeCalls, FilePath: repo + "/caller.cs", Line: 4, Origin: graph.OriginASTResolved},
	})
	return caller, ifaceMethod, implMethod
}

func ifaceDispatchCount(g graph.Store, caller, target string) int {
	count := 0
	for _, edge := range g.GetOutEdges(caller) {
		if edge != nil && edge.To == target && isIfaceDispatchEdge(edge) {
			count++
		}
	}
	return count
}

func TestCSharpInterfaceDispatchScopedMatchesFullAndUsesBatchFrontier(t *testing.T) {
	full := graph.New()
	fullCaller, _, fullImpl := addCSharpDispatchRepo(full, "changed")
	addCSharpDispatchRepo(full, "untouched")
	require.Equal(t, 2, ResolveCSharpInterfaceDispatch(full))
	require.Equal(t, 1, ifaceDispatchCount(full, fullCaller, fullImpl))

	partial := graph.New()
	partialCaller, _, partialImpl := addCSharpDispatchRepo(partial, "changed")
	otherCaller, _, otherImpl := addCSharpDispatchRepo(partial, "untouched")
	counting := &frameworkTailCountingStore{Store: partial}
	require.Equal(t, 1, ResolveCSharpInterfaceDispatchScoped(counting, map[string]bool{"changed": true}))
	require.Equal(t, 1, ifaceDispatchCount(partial, partialCaller, partialImpl), "changed-family parity with full pass")
	require.Zero(t, ifaceDispatchCount(partial, otherCaller, otherImpl), "untouched family stays outside frontier")
	require.Zero(t, counting.getNode)
	require.Zero(t, counting.getInEdges)
	require.Zero(t, counting.addEdge)
	require.Equal(t, 1, counting.getInEdgesByIDs)
	require.Equal(t, 1, counting.addBatch)
}

func addReceiverGateRepo(g *graph.Graph, repo string) (caller, target string) {
	caller = repo + "::Caller.Run"
	target = repo + "::Other.Do"
	g.AddBatch([]*graph.Node{
		{ID: caller, Kind: graph.KindMethod, Name: "Run", Language: "csharp", RepoPrefix: repo, Meta: map[string]any{"receiver": "Caller"}},
		{ID: target, Kind: graph.KindMethod, Name: "Do", Language: "csharp", RepoPrefix: repo, Meta: map[string]any{"receiver": "Other"}},
		{ID: repo + "::Receiver", Kind: graph.KindType, Name: "Receiver", Language: "csharp", RepoPrefix: repo},
		{ID: repo + "::Other", Kind: graph.KindType, Name: "Other", Language: "csharp", RepoPrefix: repo},
	}, []*graph.Edge{{From: caller, To: target, Kind: graph.EdgeCalls, Origin: graph.OriginTextMatched,
		Meta: map[string]any{"receiver_type": "Receiver", "opaque": "preserved"}}})
	return caller, target
}

func TestCSharpReceiverGateScopedMatchesFullAndBatchesMutation(t *testing.T) {
	full := graph.New()
	fullCaller, fullTarget := addReceiverGateRepo(full, "changed")
	addReceiverGateRepo(full, "untouched")
	fullCounting := &frameworkTailCountingStore{Store: full}
	require.Equal(t, 2, demoteCSharpMisattributedMemberCalls(fullCounting))
	require.Equal(t, 1, fullCounting.frameworkCensusScans, "full gate should project receiver_type identities")
	fullEdge := findCallEdge(full, fullCaller, fullTarget)
	require.True(t, fullEdge.IsSpeculative())
	require.Equal(t, "preserved", fullEdge.Meta["opaque"], "exact refetch must retain opaque metadata")

	partial := graph.New()
	partialCaller, partialTarget := addReceiverGateRepo(partial, "changed")
	otherCaller, otherTarget := addReceiverGateRepo(partial, "untouched")
	counting := &frameworkTailCountingStore{Store: partial}
	require.Equal(t, 1, demoteCSharpMisattributedMemberCallsScoped(counting, map[string]bool{"changed": true}))
	require.True(t, findCallEdge(partial, partialCaller, partialTarget).IsSpeculative())
	require.False(t, findCallEdge(partial, otherCaller, otherTarget).IsSpeculative())
	require.Zero(t, counting.getNode)
	require.Zero(t, counting.getInEdges)
	require.Zero(t, counting.addEdge)
	require.Zero(t, counting.removeEdge)
	require.Equal(t, 1, counting.reindexEdges)
}

func addExactReceiverGateCall(
	g *graph.Graph,
	callerRepo, targetRepo, receiver, targetReceiver string,
) (caller, target, callerFile, targetFile, receiverType, targetType string) {
	callerFile = callerRepo + "/Caller.cs"
	targetFile = targetRepo + "/Target.cs"
	caller = callerRepo + "::Caller.Run"
	target = targetRepo + "::" + targetReceiver + ".Do"
	receiverType = callerRepo + "::" + receiver
	targetType = targetRepo + "::" + targetReceiver
	g.AddBatch([]*graph.Node{
		{ID: caller, Kind: graph.KindMethod, Name: "Run", FilePath: callerFile, Language: "csharp", RepoPrefix: callerRepo, Meta: map[string]any{"receiver": "Caller"}},
		{ID: target, Kind: graph.KindMethod, Name: "Do", FilePath: targetFile, Language: "csharp", RepoPrefix: targetRepo, Meta: map[string]any{"receiver": targetReceiver}},
		{ID: receiverType, Kind: graph.KindType, Name: receiver, FilePath: callerRepo + "/Types.cs", Language: "csharp", RepoPrefix: callerRepo},
		{ID: targetType, Kind: graph.KindType, Name: targetReceiver, FilePath: targetRepo + "/Types.cs", Language: "csharp", RepoPrefix: targetRepo},
	}, []*graph.Edge{{
		From: caller, To: target, Kind: graph.EdgeCalls, FilePath: callerFile,
		Origin: graph.OriginTextMatched, Meta: map[string]any{"receiver_type": receiver},
	}})
	return caller, target, callerFile, targetFile, receiverType, targetType
}

func TestCSharpReceiverGateForFilesUsesExactSourceFrontier(t *testing.T) {
	g := graph.New()
	caller, target, callerFile, _, _, _ := addExactReceiverGateCall(g, "changed", "changed", "Receiver", "Other")
	counting := &frameworkTailCountingStore{Store: g}

	require.Equal(t, 1, demoteCSharpMisattributedMemberCallsScopedForFiles(
		counting, map[string]bool{"changed": true}, []string{callerFile}, false,
	))
	require.True(t, findCallEdge(g, caller, target).IsSpeculative())
	require.Zero(t, counting.repoEdgesByKinds)
	require.Zero(t, counting.repoNodeIDsByKind)
}

func TestCSharpReceiverGateForFilesIncludesIncomingChangedMethodCalls(t *testing.T) {
	g := graph.New()
	caller, target, _, targetFile, _, _ := addExactReceiverGateCall(g, "caller", "changed", "Receiver", "Other")
	counting := &frameworkTailCountingStore{Store: g}

	require.Equal(t, 1, demoteCSharpMisattributedMemberCallsScopedForFiles(
		counting, map[string]bool{"changed": true}, []string{targetFile}, false,
	))
	require.True(t, findCallEdge(g, caller, target).IsSpeculative())
	require.Zero(t, counting.repoEdgesByKinds)
	require.Zero(t, counting.repoNodeIDsByKind)
}

func TestCSharpReceiverGateForFilesWalksExactTransitiveHierarchy(t *testing.T) {
	g := graph.New()
	caller, target, callerFile, _, receiverType, targetType := addExactReceiverGateCall(g, "changed", "changed", "Derived", "Base")
	middle := "changed::Middle"
	g.AddNode(&graph.Node{ID: middle, Kind: graph.KindType, Name: "Middle", FilePath: "changed/Types.cs", Language: "csharp", RepoPrefix: "changed"})
	g.AddBatch(nil, []*graph.Edge{
		{From: receiverType, To: middle, Kind: graph.EdgeExtends},
		{From: middle, To: targetType, Kind: graph.EdgeExtends},
	})

	require.Zero(t, demoteCSharpMisattributedMemberCallsScopedForFiles(
		g, map[string]bool{"changed": true}, []string{callerFile}, false,
	))
	require.False(t, findCallEdge(g, caller, target).IsSpeculative())
}

func TestCSharpReceiverGateForFilesKeepsIncompleteHierarchy(t *testing.T) {
	g := graph.New()
	caller, target, callerFile, _, receiverType, _ := addExactReceiverGateCall(g, "changed", "changed", "Derived", "Other")
	g.AddEdge(&graph.Edge{From: receiverType, To: "unresolved::ExternalBase", Kind: graph.EdgeExtends})

	require.Zero(t, demoteCSharpMisattributedMemberCallsScopedForFiles(
		g, map[string]bool{"changed": true}, []string{callerFile}, false,
	))
	require.False(t, findCallEdge(g, caller, target).IsSpeculative())
}

func TestCSharpReceiverGateForFilesFallsBackForHierarchyChanges(t *testing.T) {
	g := graph.New()
	_, _, callerFile, _, _, _ := addExactReceiverGateCall(g, "changed", "changed", "Receiver", "Other")
	counting := &frameworkTailCountingStore{Store: g}

	require.Equal(t, 1, demoteCSharpMisattributedMemberCallsScopedForFiles(
		counting, map[string]bool{"changed": true}, []string{callerFile}, true,
	))
	require.Positive(t, counting.repoEdgesByKinds)
	require.Positive(t, counting.repoNodeIDsByKind)
}

func TestFrameworkFamilyGateScopedDeletesExactChangedEdgesInOneBatch(t *testing.T) {
	g := graph.New()
	for _, repo := range []string{"changed", "untouched"} {
		from := repo + "::Page"
		to := repo + "::Counter"
		g.AddBatch([]*graph.Node{
			{ID: from, Kind: graph.KindType, Name: "Page", Language: "razor", RepoPrefix: repo},
			{ID: to, Kind: graph.KindType, Name: "Counter", Language: "typescript", RepoPrefix: repo},
		}, []*graph.Edge{{From: from, To: to, Kind: graph.EdgeReferences, Meta: map[string]any{MetaSynthesizedBy: SynthRustScope}}})
	}
	counting := &frameworkTailCountingStore{Store: g}
	require.Equal(t, 1, applyFrameworkFamilyGateScoped(counting, map[string]bool{"changed": true}))
	require.Equal(t, 1, counting.removeEdgesExact)
	require.Zero(t, counting.removeEdge)
	require.Empty(t, g.GetOutEdges("changed::Page"))
	require.Len(t, g.GetOutEdges("untouched::Page"), 1)
}
