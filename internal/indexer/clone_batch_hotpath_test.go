package indexer

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
)

// cloneIOCountingStore proves clone materialisation uses the Store's set-shaped
// APIs. The embedded store supplies the complete interface while these four
// overrides make a per-pair point read/write regression immediately visible.
type cloneIOCountingStore struct {
	graph.Store
	getNodeCalls       int
	getNodesBatchCalls int
	addEdgeCalls       int
	addBatchCalls      int
	batchEdges         int
	maxLookupIDs       int
	maxBatchEdges      int
}

func (s *cloneIOCountingStore) GetNode(id string) *graph.Node {
	s.getNodeCalls++
	return s.Store.GetNode(id)
}

func (s *cloneIOCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesBatchCalls++
	if len(ids) > s.maxLookupIDs {
		s.maxLookupIDs = len(ids)
	}
	return s.Store.GetNodesByIDs(ids)
}

func (s *cloneIOCountingStore) AddEdge(edge *graph.Edge) {
	s.addEdgeCalls++
	s.Store.AddEdge(edge)
}

func (s *cloneIOCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatchCalls++
	s.batchEdges += len(edges)
	if len(edges) > s.maxBatchEdges {
		s.maxBatchEdges = len(edges)
	}
	s.Store.AddBatch(nodes, edges)
}

func cloneEdgesForTest(g *graph.Graph, kind graph.EdgeKind) []*graph.Edge {
	var edges []*graph.Edge
	for edge := range g.EdgesByKinds([]graph.EdgeKind{kind}) {
		edges = append(edges, edge)
	}
	return edges
}

func TestDetectClonesBatchesEndpointReadsAndEdgeWrites(t *testing.T) {
	sig, ok := clones.ComputeSignature(cloneRepoSource)
	if !ok {
		t.Fatal("clone fixture must produce a signature")
	}
	encoded := clones.EncodeSignature(sig)

	base := graph.New()
	nodes := []*graph.Node{
		{ID: "a.go::A", Kind: graph.KindFunction, FilePath: "a.go", StartLine: 11, Meta: map[string]any{cloneSigMetaKey: encoded}},
		{ID: "b.go::B", Kind: graph.KindFunction, FilePath: "b.go", StartLine: 22, Meta: map[string]any{cloneSigMetaKey: encoded}},
		{ID: "c.go::C", Kind: graph.KindMethod, FilePath: "c.go", StartLine: 33, Meta: map[string]any{cloneSigMetaKey: encoded}},
	}
	base.AddBatch(nodes, nil)
	store := &cloneIOCountingStore{Store: base}

	stats, _ := detectClonesAndEmitEdgesWithBaselineCtx(context.Background(), store, "", 0)
	if stats.Items != 3 || stats.Pairs != 3 || stats.Edges != 6 {
		t.Fatalf("clone stats = items:%d pairs:%d edges:%d, want 3/3/6", stats.Items, stats.Pairs, stats.Edges)
	}
	if store.getNodeCalls != 0 || store.addEdgeCalls != 0 {
		t.Fatalf("per-pair operations = %d GetNode / %d AddEdge, want 0/0", store.getNodeCalls, store.addEdgeCalls)
	}
	if store.getNodesBatchCalls != 1 || store.addBatchCalls != 1 {
		t.Fatalf("batched operations = %d reads / %d writes, want 1/1", store.getNodesBatchCalls, store.addBatchCalls)
	}
	if store.batchEdges != 6 {
		t.Fatalf("batched edges = %d, want 6", store.batchEdges)
	}

	byID := make(map[string]*graph.Node, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}
	for _, edge := range cloneEdgesForTest(base, graph.EdgeSimilarTo) {
		source := byID[edge.From]
		if source == nil {
			t.Fatalf("edge source %q missing", edge.From)
		}
		if edge.Kind != graph.EdgeSimilarTo || edge.FilePath != source.FilePath || edge.Line != source.StartLine {
			t.Fatalf("edge locality/kind changed: %+v", edge)
		}
		if edge.Confidence != 1 || edge.Origin != graph.OriginASTInferred || edge.Meta["similarity"] != float64(1) {
			t.Fatalf("edge confidence/provenance metadata changed: %+v", edge)
		}
	}
}

func TestMaterializeClonePairsBoundsReadAndWriteBatches(t *testing.T) {
	const pairCount = clonePairBatchSize + 1
	base := graph.New()
	nodes := make([]*graph.Node, 0, pairCount+1)
	nodes = append(nodes, &graph.Node{ID: "hub", Kind: graph.KindFunction, FilePath: "hub.go", StartLine: 7})
	pairs := make([]clones.Pair, 0, pairCount)
	for i := 0; i < pairCount; i++ {
		id := fmt.Sprintf("leaf-%04d", i)
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, FilePath: id + ".go", StartLine: i + 1})
		pairs = append(pairs, clones.Pair{A: "hub", B: id, Similarity: 0.9})
	}
	base.AddBatch(nodes, nil)
	store := &cloneIOCountingStore{Store: base}

	materialized, logicalEdges := materializeClonePairs(store, pairs, graph.EdgeSimilarTo, nil)
	if materialized != pairCount || logicalEdges != 2*pairCount {
		t.Fatalf("materialized = %d pairs/%d edges, want %d/%d", materialized, logicalEdges, pairCount, 2*pairCount)
	}
	if store.getNodeCalls != 0 || store.addEdgeCalls != 0 {
		t.Fatalf("per-pair operations = %d GetNode / %d AddEdge, want 0/0", store.getNodeCalls, store.addEdgeCalls)
	}
	if store.getNodesBatchCalls != 2 || store.addBatchCalls != 2 {
		t.Fatalf("bounded operations = %d reads / %d writes, want 2/2", store.getNodesBatchCalls, store.addBatchCalls)
	}
	if store.maxLookupIDs > 2*clonePairBatchSize || store.maxBatchEdges > 2*clonePairBatchSize {
		t.Fatalf("batch bounds exceeded: max lookup=%d max edges=%d", store.maxLookupIDs, store.maxBatchEdges)
	}
	if store.batchEdges != 2*pairCount {
		t.Fatalf("persisted edges = %d, want %d", store.batchEdges, 2*pairCount)
	}
}

func TestCloneDiffusionBatchesEndpointReadsAndWrites(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "A", Kind: graph.KindFunction, FilePath: "a.go", StartLine: 4},
		{ID: "B", Kind: graph.KindFunction, FilePath: "b.go", StartLine: 8},
		{ID: "C", Kind: graph.KindFunction, FilePath: "c.go", StartLine: 12},
	}, nil)
	store := &cloneIOCountingStore{Store: base}
	pairs := []clones.Pair{
		{A: "A", B: "B", Similarity: 0.9},
		{A: "B", B: "C", Similarity: 0.9},
	}
	direct := map[[2]string]struct{}{
		canonicalPair("A", "B"): {},
		canonicalPair("B", "C"): {},
	}

	diffusedPairs, diffusedEdges := diffuseSimilarityEdges(store, pairs, direct)
	if diffusedPairs != 1 || diffusedEdges != 2 {
		t.Fatalf("diffusion = %d pairs/%d edges, want 1/2", diffusedPairs, diffusedEdges)
	}
	if store.getNodeCalls != 0 || store.addEdgeCalls != 0 {
		t.Fatalf("diffusion per-pair operations = %d GetNode / %d AddEdge, want 0/0", store.getNodeCalls, store.addEdgeCalls)
	}
	if store.getNodesBatchCalls != 1 || store.addBatchCalls != 1 || store.batchEdges != 2 {
		t.Fatalf("diffusion batched operations = %d reads / %d writes / %d edges, want 1/1/2", store.getNodesBatchCalls, store.addBatchCalls, store.batchEdges)
	}
	for _, edge := range cloneEdgesForTest(base, graph.EdgeSemanticallyRelated) {
		if edge.Kind != graph.EdgeSemanticallyRelated || edge.Origin != graph.OriginASTInferred {
			t.Fatalf("diffused edge semantics changed: %+v", edge)
		}
		wantScore := diffusionDamping * pairs[0].Similarity * pairs[1].Similarity
		if edge.Confidence != wantScore || edge.Meta["similarity"] != wantScore {
			t.Fatalf("diffused score = %v/%v, want %v", edge.Confidence, edge.Meta["similarity"], wantScore)
		}
	}
}

func TestIncrementalCloneUpdatePrefetchesAndDedupesPairs(t *testing.T) {
	shingles, tokens, ok := clones.Shingles(cloneRepoSource)
	if !ok {
		t.Fatal("clone fixture must produce shingles")
	}
	sig, ok := computeCloneSigFromShingles(nil, 0, false, shingles)
	if !ok {
		t.Fatal("clone fixture must produce a signature")
	}
	encoded := clones.EncodeSignature(sig)

	old := &graph.Node{
		ID: "old.go::Old", Kind: graph.KindFunction, FilePath: "old.go", StartLine: 3,
		Meta: map[string]any{cloneSigMetaKey: encoded, cloneTokensMetaKey: tokens},
	}
	newA := &graph.Node{
		ID: "new.go::A", Kind: graph.KindFunction, FilePath: "new.go", StartLine: 10,
		Meta: map[string]any{cloneShinglesMetaKey: shingles, cloneTokensMetaKey: tokens},
	}
	newB := &graph.Node{
		ID: "new.go::B", Kind: graph.KindMethod, FilePath: "new.go", StartLine: 40,
		Meta: map[string]any{cloneShinglesMetaKey: shingles, cloneTokensMetaKey: tokens},
	}
	base := graph.New()
	base.AddBatch([]*graph.Node{old, newA, newB}, nil)
	store := &cloneIOCountingStore{Store: base}

	ci := newIncrementalCloneIndex()
	ci.cms = clones.NewCMS(65536, 4)
	ci.lsh = clones.NewStratifiedIndex()
	ci.shingles = make(map[string][]uint64)
	for _, shingle := range shingles {
		ci.cms.Add(shingle)
	}
	ci.shingles[old.ID] = shingles
	ci.corpus = 1
	ci.built = true
	ci.lsh.Add(clones.Item{ID: old.ID, Sig: sig, TokenCount: tokens})

	ci.UpdateFuncs(store, "", []*graph.Node{newA, newB}, 0)
	if store.getNodeCalls != 0 || store.addEdgeCalls != 0 {
		t.Fatalf("partial per-pair operations = %d GetNode / %d AddEdge, want 0/0", store.getNodeCalls, store.addEdgeCalls)
	}
	if store.getNodesBatchCalls != 1 || store.addBatchCalls != 1 {
		t.Fatalf("partial batched operations = %d reads / %d writes, want 1/1", store.getNodesBatchCalls, store.addBatchCalls)
	}
	// Three undirected pairs exist among the three identical bodies. The
	// newA-newB pair is returned once from each new endpoint, but must be
	// written only once in each direction.
	stored := cloneEdgesForTest(base, graph.EdgeSimilarTo)
	if store.batchEdges != 6 || len(stored) != 6 {
		t.Fatalf("partial edges = %d staged/%d stored, want 6/6", store.batchEdges, len(stored))
	}
	for _, edge := range stored {
		if edge.Kind != graph.EdgeSimilarTo || edge.Origin != graph.OriginASTInferred || edge.Meta["similarity"] != float64(1) {
			t.Fatalf("partial edge semantics changed: %+v", edge)
		}
	}
}
