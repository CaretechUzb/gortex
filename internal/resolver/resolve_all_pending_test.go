package resolver

import (
	"iter"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type lazyResolveCountingStore struct {
	graph.Store

	nodesByKind     map[graph.NodeKind]int
	edgesByKind     map[graph.EdgeKind]int
	getRepoNodes    map[string]int
	allNodes        int
	allEdges        int
	unresolvedScan  int
	reindexWrites   int
	backendBulk     int
	scopedBulk      int
	backendPending  bool
	backendCreate   func()
	scopedNodeRepos [][]string
}

func newLazyResolveCountingStore(store graph.Store) *lazyResolveCountingStore {
	return &lazyResolveCountingStore{
		Store:        store,
		nodesByKind:  make(map[graph.NodeKind]int),
		edgesByKind:  make(map[graph.EdgeKind]int),
		getRepoNodes: make(map[string]int),
	}
}

func (s *lazyResolveCountingStore) NodesByKind(kind graph.NodeKind) iter.Seq[*graph.Node] {
	s.nodesByKind[kind]++
	return s.Store.NodesByKind(kind)
}

func (s *lazyResolveCountingStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	s.edgesByKind[kind]++
	return s.Store.EdgesByKind(kind)
}

func (s *lazyResolveCountingStore) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	s.unresolvedScan++
	return s.Store.EdgesWithUnresolvedTarget()
}

func (s *lazyResolveCountingStore) GetRepoNodes(prefix string) []*graph.Node {
	s.getRepoNodes[prefix]++
	return s.Store.GetRepoNodes(prefix)
}

func (s *lazyResolveCountingStore) AllNodes() []*graph.Node {
	s.allNodes++
	return s.Store.AllNodes()
}

func (s *lazyResolveCountingStore) AllEdges() []*graph.Edge {
	s.allEdges++
	return s.Store.AllEdges()
}

func (s *lazyResolveCountingStore) NodesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.NodeKind) iter.Seq[*graph.Node] {
	s.scopedNodeRepos = append(s.scopedNodeRepos, append([]string(nil), repoPrefixes...))
	return graph.NodesInScopeSeq(s.Store, repoPrefixes, filePaths, kinds...)
}

func (s *lazyResolveCountingStore) EdgesInScopeSeq(repoPrefixes, filePaths []string, kinds ...graph.EdgeKind) iter.Seq[graph.ScopedEdgeRow] {
	return graph.EdgesInScopeSeq(s.Store, repoPrefixes, filePaths, kinds...)
}

func (s *lazyResolveCountingStore) NodesLightInScopeSeq(repoPrefixes, filePaths []string) iter.Seq[*graph.Node] {
	return graph.NodesLightInScopeSeq(s.Store, repoPrefixes, filePaths)
}

func (s *lazyResolveCountingStore) ReindexEdge(edge *graph.Edge, oldTo string) {
	s.reindexWrites++
	s.Store.ReindexEdge(edge, oldTo)
}

func (s *lazyResolveCountingStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexWrites += len(batch)
	s.Store.ReindexEdges(batch)
}

func (s *lazyResolveCountingStore) BackendResolveWorkPending([]string) (bool, error) {
	return s.backendPending, nil
}

func (s *lazyResolveCountingStore) ResolveAllBulk() (int, error) {
	s.backendBulk++
	if s.backendCreate != nil {
		s.backendCreate()
		s.backendCreate = nil
		s.backendPending = false
	}
	return 0, nil
}

func (s *lazyResolveCountingStore) ResolveAllBulkScoped([]string) (int, error) {
	s.scopedBulk++
	return 0, nil
}

func (s *lazyResolveCountingStore) ResolveSameFile() (int, error)    { return 0, nil }
func (s *lazyResolveCountingStore) ResolveSamePackage() (int, error) { return 0, nil }
func (s *lazyResolveCountingStore) ResolveImportAware() (int, error) { return 0, nil }
func (s *lazyResolveCountingStore) ResolveRelativeImports(string) (int, error) {
	return 0, nil
}
func (s *lazyResolveCountingStore) ResolveCrossRepo() (int, error)         { return 0, nil }
func (s *lazyResolveCountingStore) ResolveUniqueNames() (int, error)       { return 0, nil }
func (s *lazyResolveCountingStore) ResolveExternalCallStubs() (int, error) { return 0, nil }

func TestPendingNeedsProvidesIndexAcceptsSparsePreparationEdge(t *testing.T) {
	sparse := &graph.Edge{
		From: "repo::caller", To: "unresolved::*.Run", Kind: graph.EdgeCalls,
		FilePath: "repo/caller.cs", Line: 7,
	}
	if !pendingNeedsProvidesIndex([]*graph.Edge{sparse}) {
		t.Fatal("wildcard identity-only preparation edge did not enable provides index")
	}
	if pendingNeedsProvidesIndex([]*graph.Edge{{To: "unresolved::Run"}}) {
		t.Fatal("bare target unexpectedly enabled provides index")
	}
}

func TestResolveAllZeroWorkSkipsGlobalIndexesAndBackendBulk(t *testing.T) {
	store := newLazyResolveCountingStore(graph.New())
	stats := New(store).ResolveAll()
	if stats.PendingBefore != 0 || stats.PendingAfter != 0 {
		t.Fatalf("pending stats = %d/%d, want 0/0", stats.PendingBefore, stats.PendingAfter)
	}
	if store.backendBulk != 0 || store.scopedBulk != 0 {
		t.Fatalf("backend bulk calls = unscoped:%d scoped:%d, want 0/0", store.backendBulk, store.scopedBulk)
	}
	if store.allNodes != 0 || store.allEdges != 0 || len(store.nodesByKind) != 0 || len(store.edgesByKind) != 0 {
		t.Fatalf("zero-work global scans: all_nodes=%d all_edges=%d nodes_by_kind=%v edges_by_kind=%v",
			store.allNodes, store.allEdges, store.nodesByKind, store.edgesByKind)
	}
	if store.unresolvedScan != 1 {
		t.Fatalf("pending scans = %d, want exactly 1", store.unresolvedScan)
	}
}

func TestResolveAllScopedUsesPendingRepoFrontierAndNeverUnscopedBulk(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a::src/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a::src/caller.go", RepoPrefix: "a"},
		{ID: "a::src/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a::src/caller.go", RepoPrefix: "a"},
		{ID: "a::src/work.go", Kind: graph.KindFile, Name: "work.go", FilePath: "a::src/work.go", RepoPrefix: "a"},
		{ID: "a::src/work.go::Work", Kind: graph.KindFunction, Name: "Work", FilePath: "a::src/work.go", RepoPrefix: "a"},
		{ID: "b::other.go", Kind: graph.KindFile, Name: "other.go", FilePath: "b::other.go", RepoPrefix: "b"},
		{ID: "b::other.go::Other", Kind: graph.KindFunction, Name: "Other", FilePath: "b::other.go", RepoPrefix: "b"},
	}, []*graph.Edge{{
		From: "a::src/caller.go::Caller", To: graph.UnresolvedMarker + "Work",
		Kind: graph.EdgeCalls, FilePath: "a::src/caller.go", Line: 3,
	}})

	store := newLazyResolveCountingStore(g)
	r := New(store)
	r.SetScope(map[string]struct{}{"a": {}})
	stats := r.ResolveAll()
	if stats.Resolved != 1 || stats.PendingAfter != 1 {
		// PendingAfter is the number loaded into this scoped pass, not the
		// number left unresolved after reindexing.
		t.Fatalf("stats = %+v, want one resolved scoped edge", stats)
	}
	if store.backendBulk != 0 {
		t.Fatalf("scoped ResolveAll called unscoped backend bulk %d time(s)", store.backendBulk)
	}
	if store.scopedBulk != 1 {
		t.Fatalf("scoped backend bulk calls = %d, want 1", store.scopedBulk)
	}
	if store.nodesByKind[graph.KindContract] != 0 || store.edgesByKind[graph.EdgeProvides] != 0 {
		t.Fatalf("scoped pass built unused dep/provides indexes: contract=%d provides=%d",
			store.nodesByKind[graph.KindContract], store.edgesByKind[graph.EdgeProvides])
	}
	if store.getRepoNodes["b"] != 0 {
		t.Fatalf("scoped pass materialised unrelated repo b %d time(s)", store.getRepoNodes["b"])
	}
	if len(store.scopedNodeRepos) == 0 {
		t.Fatal("scoped pass did not use a node projection")
	}
	sawRepoProjection := false
	for _, prefixes := range store.scopedNodeRepos {
		for _, prefix := range prefixes {
			if prefix == "a" {
				sawRepoProjection = true
			}
			if prefix != "a" && !strings.HasPrefix(prefix, "a::") {
				t.Fatalf("scoped projection escaped pending frontier: %v", prefixes)
			}
		}
	}
	if !sawRepoProjection {
		t.Fatalf("directory index never projected repo a: %v", store.scopedNodeRepos)
	}
	out := g.GetOutEdges("a::src/caller.go::Caller")
	if len(out) != 1 || out[0].To != "a::src/work.go::Work" {
		t.Fatalf("resolved edge = %#v", out)
	}
}

func TestResolveAllBackendCreatedPendingWorkIsReopenedAndResolved(t *testing.T) {
	g := graph.New()
	store := newLazyResolveCountingStore(g)
	store.backendPending = true
	store.backendCreate = func() {
		g.AddBatch([]*graph.Node{
			{ID: "caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "caller.go"},
			{ID: "work.go::Work", Kind: graph.KindFunction, Name: "Work", FilePath: "work.go"},
		}, []*graph.Edge{{From: "caller.go::Caller", To: graph.UnresolvedMarker + "Work", Kind: graph.EdgeCalls, FilePath: "caller.go"}})
	}

	stats := New(store).ResolveAll()
	if store.backendBulk != 1 || stats.Resolved != 1 {
		t.Fatalf("backend bulk=%d stats=%+v, want one created edge resolved", store.backendBulk, stats)
	}
	out := g.GetOutEdges("caller.go::Caller")
	if len(out) != 1 || out[0].To != "work.go::Work" {
		t.Fatalf("backend-created edge = %#v", out)
	}
}

func TestResolveAllRepeatedWarmCallDoesNoGlobalWorkOrWrites(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "caller.go"},
		{ID: "work.go::Work", Kind: graph.KindFunction, Name: "Work", FilePath: "work.go"},
	}, []*graph.Edge{{From: "caller.go::Caller", To: graph.UnresolvedMarker + "Work", Kind: graph.EdgeCalls, FilePath: "caller.go"}})
	store := newLazyResolveCountingStore(g)
	r := New(store)
	if first := r.ResolveAll(); first.Resolved != 1 {
		t.Fatalf("first stats = %+v", first)
	}
	bulk, writes := store.backendBulk, store.reindexWrites
	allNodes, allEdges := store.allNodes, store.allEdges
	nodeKinds, edgeKinds := len(store.nodesByKind), len(store.edgesByKind)
	if second := r.ResolveAll(); second.PendingBefore != 0 || second.PendingAfter != 0 {
		t.Fatalf("second stats = %+v, want zero pending", second)
	}
	if store.backendBulk != bulk || store.reindexWrites != writes || store.allNodes != allNodes || store.allEdges != allEdges ||
		len(store.nodesByKind) != nodeKinds || len(store.edgesByKind) != edgeKinds {
		t.Fatalf("warm repeat added work: bulk %d->%d writes %d->%d all %d/%d->%d/%d kinds %d/%d->%d/%d",
			bulk, store.backendBulk, writes, store.reindexWrites, allNodes, allEdges, store.allNodes, store.allEdges,
			nodeKinds, edgeKinds, len(store.nodesByKind), len(store.edgesByKind))
	}
}

func TestResolveAllPassIndexesReuseFullScansAcrossPages(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "caller.go"},
		{ID: "caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "caller.go"},
		{ID: "module.go::Module", Kind: graph.KindType, Name: "Module", FilePath: "module.go"},
		{ID: "dep::example.com/lib", Kind: graph.KindContract, Name: "example.com/lib"},
	}, []*graph.Edge{{
		From: "module.go::Module", To: graph.UnresolvedMarker + "Concrete",
		Kind: graph.EdgeProvides, FilePath: "module.go",
		Meta: map[string]any{"provides_for": "Abstract", "binding": "useClass"},
	}})

	store := newLazyResolveCountingStore(g)
	r := New(store)
	indexes := newResolveAllPassIndexes(r)
	defer indexes.close()

	pending := make([]*graph.Edge, 2*resolvePendingPageRows+1)
	for i := range pending {
		if i%2 == 0 {
			pending[i] = &graph.Edge{
				From: "caller.go::Caller", To: graph.UnresolvedMarker + "import::example.com/lib/pkg",
				Kind: graph.EdgeImports, FilePath: "caller.go",
			}
		} else {
			pending[i] = &graph.Edge{
				From: "caller.go::Caller", To: graph.UnresolvedMarker + "*.Run",
				Kind: graph.EdgeCalls, FilePath: "caller.go", Meta: map[string]any{"receiver_type": "Abstract"},
			}
		}
	}
	for start := 0; start < len(pending); start += resolvePendingPageRows {
		end := start + resolvePendingPageRows
		if end > len(pending) {
			end = len(pending)
		}
		indexes.prepare(pending[start:end])
		indexes.clearPage()
	}

	if store.nodesByKind[graph.KindFile] != 1 || store.nodesByKind[graph.KindContract] != 1 || store.edgesByKind[graph.EdgeProvides] != 1 {
		t.Fatalf("full index scans across >2 pages: file=%d contract=%d provides=%d, want 1/1/1",
			store.nodesByKind[graph.KindFile], store.nodesByKind[graph.KindContract], store.edgesByKind[graph.EdgeProvides])
	}
}

func TestBuildPassIndexesForOneFileUsesOnlyItsRepoProjection(t *testing.T) {
	g := graph.New()
	g.AddBatch([]*graph.Node{
		{ID: "a::src/caller.go", Kind: graph.KindFile, Name: "caller.go", FilePath: "a::src/caller.go", RepoPrefix: "a"},
		{ID: "a::src/caller.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "a::src/caller.go", RepoPrefix: "a"},
		{ID: "dep::example.com/lib", Kind: graph.KindContract, Name: "example.com/lib", RepoPrefix: "a"},
		{ID: "b::unrelated.go", Kind: graph.KindFile, Name: "unrelated.go", FilePath: "b::unrelated.go", RepoPrefix: "b"},
	}, nil)
	store := newLazyResolveCountingStore(g)
	pending := []*graph.Edge{{
		From: "a::src/caller.go::Caller", To: graph.UnresolvedMarker + "import::example.com/lib/pkg",
		Kind: graph.EdgeImports, FilePath: "a::src/caller.go",
	}}
	clear := New(store).buildPassIndexesForPending(pending)
	clear()

	if store.nodesByKind[graph.KindFile] != 0 || store.nodesByKind[graph.KindContract] != 0 {
		t.Fatalf("one-file frontier used global kind scans: file=%d contract=%d",
			store.nodesByKind[graph.KindFile], store.nodesByKind[graph.KindContract])
	}
	if store.getRepoNodes["b"] != 0 {
		t.Fatalf("one-file frontier materialised unrelated repo b %d time(s)", store.getRepoNodes["b"])
	}
	for _, prefixes := range store.scopedNodeRepos {
		for _, prefix := range prefixes {
			if prefix != "a" {
				t.Fatalf("one-file frontier escaped repo a: %v", store.scopedNodeRepos)
			}
		}
	}
}
