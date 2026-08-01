package resolver

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// resolveInterleavePagingStore keeps the cold pending page stable while
// recording any point lookup that would leak after an interleaving incremental
// pass clears the Resolver's per-pass caches.
type resolveInterleavePagingStore struct {
	*resolverBatchCountingStore

	pageEdges []*graph.Edge

	beginCalls                     int
	pageCalls                      int
	findNodesByNameCalls           int
	findNodesByNameInRepoCalls     int
	findNodesByNameContainingCalls int
	getNodeByQualNameCalls         int
	findNodesByNamesCalls          int
	getEdgeCandidatesCalls         int
	nameWarmsAtCandidateCalls      []int
	edgeExistsCalls                int
}

func (s *resolveInterleavePagingStore) EdgeMutationRevision() uint64 {
	return s.resolverBatchCountingStore.Store.(edgeMutationRevisioner).EdgeMutationRevision()
}

func (s *resolveInterleavePagingStore) MutationRevision() uint64 {
	return s.resolverBatchCountingStore.Store.(mutationRevisioner).MutationRevision()
}

func (s *resolveInterleavePagingStore) BeginUnresolvedEdgeScan(context.Context) (graph.UnresolvedEdgeScan, error) {
	s.beginCalls++
	return graph.UnresolvedEdgeScan{
		HighWaterID:   int64(len(s.pageEdges)),
		PendingBefore: len(s.pageEdges),
	}, nil
}

func (s *resolveInterleavePagingStore) ReadUnresolvedEdgePage(
	_ context.Context, _ graph.UnresolvedEdgeScan, afterID int64, _, _ int,
) (graph.UnresolvedEdgePage, error) {
	s.pageCalls++
	if afterID >= int64(len(s.pageEdges)) {
		return graph.UnresolvedEdgePage{NextID: afterID, Exhausted: true}, nil
	}
	return graph.UnresolvedEdgePage{
		Edges:     append([]*graph.Edge(nil), s.pageEdges...),
		NextID:    int64(len(s.pageEdges)),
		Exhausted: true,
	}, nil
}

func (s *resolveInterleavePagingStore) FindNodesByName(name string) []*graph.Node {
	s.findNodesByNameCalls++
	return s.Store.FindNodesByName(name)
}

func (s *resolveInterleavePagingStore) FindNodesByNameInRepo(name, repoPrefix string) []*graph.Node {
	s.findNodesByNameInRepoCalls++
	return s.Store.FindNodesByNameInRepo(name, repoPrefix)
}

func (s *resolveInterleavePagingStore) FindNodesByNameContaining(substr string, limit int) []*graph.Node {
	s.findNodesByNameContainingCalls++
	return s.Store.FindNodesByNameContaining(substr, limit)
}

func (s *resolveInterleavePagingStore) GetNodeByQualName(qualName string) *graph.Node {
	s.getNodeByQualNameCalls++
	return s.Store.GetNodeByQualName(qualName)
}

func (s *resolveInterleavePagingStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	s.findNodesByNamesCalls++
	return s.Store.FindNodesByNames(names)
}

func (s *resolveInterleavePagingStore) FindNodesByNamesInRepoLanguages(
	names []string, repo string, languages []string,
) map[string][]*graph.Node {
	s.findNodesByNamesCalls++
	return graph.FindNodesByNamesInRepoLanguages(s.Store, names, repo, languages)
}

func (s *resolveInterleavePagingStore) GetEdgeCandidates(
	endpoints []graph.EdgeEndpoint, sites []graph.EdgeSite,
) graph.EdgeCandidateSet {
	s.getEdgeCandidatesCalls++
	s.nameWarmsAtCandidateCalls = append(s.nameWarmsAtCandidateCalls, s.findNodesByNamesCalls)
	return s.Store.GetEdgeCandidates(endpoints, sites)
}

// EdgeExists is not part of graph.Store, but retaining the optional point
// capability here makes the regression fail if chunk liveness ever falls back
// from GetEdgeCandidates to one existence check per edge.
func (s *resolveInterleavePagingStore) EdgeExists(
	from, to string, kind graph.EdgeKind, filePath string, line int,
) bool {
	s.edgeExistsCalls++
	for _, edge := range s.Store.GetOutEdges(from) {
		if edge.To == to && edge.Kind == kind && edge.FilePath == filePath && edge.Line == line {
			return true
		}
	}
	return false
}

func TestResolveAllRefreshesCurrentPageAfterSameResolverInterleave(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

	const repo = "repo"
	const language = "typescript"
	coldAPath := "pkg/cold_a.ts"
	coldBPath := "pkg/cold_b.ts"
	coldBTargetPath := "pkg/cold_b_target.ts"
	interactivePath := "pkg/interactive.ts"

	fileNode := func(path string) *graph.Node {
		return &graph.Node{
			ID: path, Kind: graph.KindFile, Name: path,
			FilePath: path, RepoPrefix: repo, Language: language,
		}
	}
	functionNode := func(id, name, path string) *graph.Node {
		return &graph.Node{
			ID: id, Kind: graph.KindFunction, Name: name,
			FilePath: path, RepoPrefix: repo, Language: language,
		}
	}

	coldACaller := functionNode("repo::cold-a-caller", "ColdACaller", coldAPath)
	coldATarget := functionNode("repo::cold-a-target", "ColdATarget", coldAPath)
	coldBCaller := functionNode("repo::cold-b-caller", "ColdBCaller", coldBPath)
	// This target deliberately lives in a neighbouring file. Correctly rebuilt
	// current-page indexes retain the same-package structural resolution tier.
	coldBTarget := functionNode("repo::cold-b-target", "ColdBTarget", coldBTargetPath)
	interactiveCaller := functionNode("repo::interactive-caller", "InteractiveCaller", interactivePath)
	interactiveTarget := functionNode("repo::interactive-target", "InteractiveTarget", interactivePath)

	coldAEdge := &graph.Edge{
		From: coldACaller.ID, To: graph.UnresolvedMarker + coldATarget.Name,
		Kind: graph.EdgeCalls, FilePath: coldAPath, Line: 10,
	}
	coldBEdge := &graph.Edge{
		From: coldBCaller.ID, To: graph.UnresolvedMarker + coldBTarget.Name,
		Kind: graph.EdgeCalls, FilePath: coldBPath, Line: 20,
	}
	interactiveEdge := &graph.Edge{
		From: interactiveCaller.ID, To: graph.UnresolvedMarker + interactiveTarget.Name,
		Kind: graph.EdgeCalls, FilePath: interactivePath, Line: 30,
	}

	g := graph.New()
	g.AddBatch([]*graph.Node{
		fileNode(coldAPath), fileNode(coldBPath), fileNode(coldBTargetPath), fileNode(interactivePath),
		coldACaller, coldATarget, coldBCaller, coldBTarget, interactiveCaller, interactiveTarget,
	}, []*graph.Edge{coldAEdge, coldBEdge, interactiveEdge})

	base := &resolverBatchCountingStore{Store: g}
	store := &resolveInterleavePagingStore{
		resolverBatchCountingStore: base,
		pageEdges:                  []*graph.Edge{coldAEdge, coldBEdge},
	}
	r := New(store)

	hookCalls := 0
	var generationBeforeHook, generationAfterHook uint64
	r.chunkYieldHook = func() {
		hookCalls++
		generationBeforeHook = r.scratchGeneration
		r.ResolveFilesAndIncoming([]string{interactivePath})
		generationAfterHook = r.scratchGeneration
	}

	stats := r.ResolveAll()

	if hookCalls != 1 {
		t.Fatalf("chunk yield hook calls=%d, want 1", hookCalls)
	}
	if generationAfterHook <= generationBeforeHook {
		t.Fatalf("incremental interleave generation=%d after %d, want an actual cache clear", generationAfterHook, generationBeforeHook)
	}
	if store.beginCalls != 1 || store.pageCalls != 1 {
		t.Fatalf("unresolved scan calls=begin:%d page:%d, want 1/1", store.beginCalls, store.pageCalls)
	}
	if stats.PendingBefore != 2 || stats.Resolved != 2 {
		t.Fatalf("cold stats pending=%d resolved=%d, want 2/2", stats.PendingBefore, stats.Resolved)
	}

	assertResolved := func(label string, edge *graph.Edge, target *graph.Node) {
		t.Helper()
		if edge.To != target.ID {
			t.Fatalf("%s target=%q, want %q", label, edge.To, target.ID)
		}
		if edge.Origin != graph.OriginASTResolved {
			t.Fatalf("%s origin=%q, want %q", label, edge.Origin, graph.OriginASTResolved)
		}
		out := g.GetOutEdges(edge.From)
		if len(out) != 1 || out[0].To != target.ID {
			t.Fatalf("%s adjacency=%#v, want one edge to %q", label, out, target.ID)
		}
	}
	assertResolved("first cold chunk", coldAEdge, coldATarget)
	assertResolved("current page after interleave", coldBEdge, coldBTarget)
	assertResolved("interactive pass", interactiveEdge, interactiveTarget)

	if store.findNodesByNamesCalls < 3 {
		t.Fatalf("batched name-cache warms=%d, want initial + interactive + post-interleave rebuild", store.findNodesByNamesCalls)
	}
	if base.getNodeCalls != 0 || base.getFileNodesCalls != 0 ||
		base.getOutEdgesCalls != 0 || base.getInEdgesCalls != 0 ||
		store.findNodesByNameCalls != 0 || store.findNodesByNameInRepoCalls != 0 ||
		store.findNodesByNameContainingCalls != 0 || store.getNodeByQualNameCalls != 0 {
		t.Fatalf("point lookups leaked: node=%d file=%d out=%d in=%d name=%d repo_name=%d contains=%d qual_name=%d",
			base.getNodeCalls, base.getFileNodesCalls, base.getOutEdgesCalls, base.getInEdgesCalls,
			store.findNodesByNameCalls, store.findNodesByNameInRepoCalls,
			store.findNodesByNameContainingCalls, store.getNodeByQualNameCalls)
	}
	if store.edgeExistsCalls != 0 {
		t.Fatalf("point EdgeExists calls=%d, want 0", store.edgeExistsCalls)
	}
	// Only the chunk after the real mutation interleave validates in the main
	// loop. One remaining-page batch covers every later chunk from the old
	// page; the second phase-bounded call is the independent cross-package
	// guard validation. Exact-site placeholder-source reconciliation shares
	// the candidate API and contributes one more batch; the old implementation
	// performed that probe through the wider adjacency API instead.
	if store.getEdgeCandidatesCalls != 3 {
		t.Fatalf("batched candidate calls=%d, want main-page + guard + placeholder-site validation", store.getEdgeCandidatesCalls)
	}
}

func TestResolveAllNoopYieldKeepsCurrentPageCaches(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1")

	const repo = "repo"
	const language = "typescript"
	paths := []string{"pkg/noop_a.ts", "pkg/noop_b.ts"}
	callers := []*graph.Node{
		{ID: "repo::noop-a-caller", Kind: graph.KindFunction, Name: "NoopACaller", FilePath: paths[0], RepoPrefix: repo, Language: language},
		{ID: "repo::noop-b-caller", Kind: graph.KindFunction, Name: "NoopBCaller", FilePath: paths[1], RepoPrefix: repo, Language: language},
	}
	targets := []*graph.Node{
		{ID: "repo::noop-a-target", Kind: graph.KindFunction, Name: "NoopATarget", FilePath: paths[0], RepoPrefix: repo, Language: language},
		{ID: "repo::noop-b-target", Kind: graph.KindFunction, Name: "NoopBTarget", FilePath: paths[1], RepoPrefix: repo, Language: language},
	}
	edges := []*graph.Edge{
		{From: callers[0].ID, To: graph.UnresolvedMarker + targets[0].Name, Kind: graph.EdgeCalls, FilePath: paths[0], Line: 10},
		{From: callers[1].ID, To: graph.UnresolvedMarker + targets[1].Name, Kind: graph.EdgeCalls, FilePath: paths[1], Line: 20},
	}

	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: paths[0], Kind: graph.KindFile, Name: paths[0], FilePath: paths[0], RepoPrefix: repo, Language: language},
		{ID: paths[1], Kind: graph.KindFile, Name: paths[1], FilePath: paths[1], RepoPrefix: repo, Language: language},
		callers[0], callers[1], targets[0], targets[1],
	}, edges)
	base := &resolverBatchCountingStore{Store: g}
	store := &resolveInterleavePagingStore{
		resolverBatchCountingStore: base,
		pageEdges:                  edges,
	}
	r := New(store)

	hookCalls := 0
	warmCallsAtYield := 0
	var generationBeforeHook, generationAfterHook uint64
	r.chunkYieldHook = func() {
		hookCalls++
		generationBeforeHook = r.scratchGeneration
		warmCallsAtYield = store.findNodesByNamesCalls
		generationAfterHook = r.scratchGeneration
	}

	stats := r.ResolveAll()
	if hookCalls != 1 {
		t.Fatalf("chunk yield hook calls=%d, want 1", hookCalls)
	}
	if generationAfterHook != generationBeforeHook {
		t.Fatalf("no-op hook changed scratch generation from %d to %d", generationBeforeHook, generationAfterHook)
	}
	if warmCallsAtYield != 1 {
		t.Fatalf("name-cache warms at yield=%d, want initial warm only", warmCallsAtYield)
	}
	if store.getEdgeCandidatesCalls != 0 {
		t.Fatalf("batched liveness calls=%d, want 0 without a mutation interleave", store.getEdgeCandidatesCalls)
	}
	if stats.PendingBefore != 2 || stats.Resolved != 2 {
		t.Fatalf("cold stats pending=%d resolved=%d, want 2/2", stats.PendingBefore, stats.Resolved)
	}
	for i, edge := range edges {
		if edge.To != targets[i].ID {
			t.Fatalf("edge %d target=%q, want %q", i, edge.To, targets[i].ID)
		}
	}
	if base.getNodeCalls != 0 || base.getFileNodesCalls != 0 ||
		base.getOutEdgesCalls != 0 || base.getInEdgesCalls != 0 ||
		store.findNodesByNameCalls != 0 || store.findNodesByNameInRepoCalls != 0 ||
		store.findNodesByNameContainingCalls != 0 || store.getNodeByQualNameCalls != 0 ||
		store.edgeExistsCalls != 0 {
		t.Fatalf("point lookups leaked across no-op yield: node=%d file=%d out=%d in=%d name=%d repo_name=%d contains=%d qual_name=%d exists=%d",
			base.getNodeCalls, base.getFileNodesCalls, base.getOutEdgesCalls, base.getInEdgesCalls,
			store.findNodesByNameCalls, store.findNodesByNameInRepoCalls,
			store.findNodesByNameContainingCalls, store.getNodeByQualNameCalls, store.edgeExistsCalls)
	}
}
