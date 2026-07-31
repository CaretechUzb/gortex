package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

type residualBatchCountingStore struct {
	graph.Store

	findNodesByNameCalls     int
	findNodesByNamesCalls    int
	getFileNodesCalls        int
	getFileNodesByPathsCalls int
	getNodeCalls             int
	getNodesByIDsCalls       int
	getOutEdgesCalls         int
	getOutEdgesByIDsCalls    int
	addNodeCalls             int
	addBatchCalls            int
}

func (s *residualBatchCountingStore) FindNodesByName(name string) []*graph.Node {
	s.findNodesByNameCalls++
	return s.Store.FindNodesByName(name)
}

func (s *residualBatchCountingStore) FindNodesByNames(names []string) map[string][]*graph.Node {
	s.findNodesByNamesCalls++
	return s.Store.FindNodesByNames(names)
}

func (s *residualBatchCountingStore) GetFileNodes(path string) []*graph.Node {
	s.getFileNodesCalls++
	return s.Store.GetFileNodes(path)
}

func (s *residualBatchCountingStore) GetFileNodesByPaths(paths []string) map[string][]*graph.Node {
	s.getFileNodesByPathsCalls++
	return s.Store.GetFileNodesByPaths(paths)
}

func (s *residualBatchCountingStore) GetNode(id string) *graph.Node {
	s.getNodeCalls++
	return s.Store.GetNode(id)
}

func (s *residualBatchCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.getNodesByIDsCalls++
	return s.Store.GetNodesByIDs(ids)
}

func (s *residualBatchCountingStore) GetOutEdges(id string) []*graph.Edge {
	s.getOutEdgesCalls++
	return s.Store.GetOutEdges(id)
}

func (s *residualBatchCountingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	s.getOutEdgesByIDsCalls++
	return s.Store.GetOutEdgesByNodeIDs(ids)
}

func (s *residualBatchCountingStore) AddNode(node *graph.Node) {
	s.addNodeCalls++
	s.Store.AddNode(node)
}

func (s *residualBatchCountingStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.addBatchCalls++
	s.Store.AddBatch(nodes, edges)
}

func TestResolveProviderHandlersUsesOneNameAndFileBatch(t *testing.T) {
	root := t.TempDir()
	source := []byte("package handlers\n\nfunc Handle() {\n\t_ = 1\n}\n")
	if err := os.WriteFile(filepath.Join(root, "handlers.go"), source, 0o644); err != nil {
		t.Fatal(err)
	}

	base := graph.New()
	handlerID := "repo/handlers.go::Handle"
	base.AddBatch([]*graph.Node{
		{ID: "repo/handlers.go::file", Kind: graph.KindFile, Name: "handlers.go", FilePath: "repo/handlers.go", Language: "go", RepoPrefix: "repo"},
		{ID: handlerID, Kind: graph.KindFunction, Name: "Handle", FilePath: "repo/handlers.go", StartLine: 3, EndLine: 5, Language: "go", RepoPrefix: "repo"},
	}, nil)
	counting := &residualBatchCountingStore{Store: base}
	idx := &Indexer{graph: counting, rootPath: root, repoPrefix: "repo", logger: zap.NewNop()}
	reg := contracts.NewRegistry()
	ids := []string{"http::one", "http::two", "http::three"}
	for _, id := range ids {
		reg.Add(contracts.Contract{
			ID: id, Type: contracts.ContractHTTP, Role: contracts.RoleProvider,
			FilePath: "repo/router.go", RepoPrefix: "repo",
			Meta: map[string]any{"handler_ident": "Handle"},
		})
	}

	idx.resolveProviderHandlers(reg)

	if counting.findNodesByNamesCalls != 1 || counting.findNodesByNameCalls != 0 {
		t.Fatalf("handler name lookups: batch=%d point=%d", counting.findNodesByNamesCalls, counting.findNodesByNameCalls)
	}
	if counting.getFileNodesByPathsCalls != 1 || counting.getFileNodesCalls != 0 {
		t.Fatalf("handler file lookups: batch=%d point=%d", counting.getFileNodesByPathsCalls, counting.getFileNodesCalls)
	}
	for _, id := range ids {
		items := reg.ByID(id)
		if len(items) != 1 {
			t.Fatalf("contract %s count=%d", id, len(items))
		}
		// SymbolID is the resolution this pass exists to produce. FilePath
		// stays on the registration site the contract was extracted from:
		// Line still describes that site, so re-homing the file would pair a
		// file and a line from two different places (issue #322).
		if items[0].SymbolID != handlerID || items[0].FilePath != "repo/router.go" {
			t.Fatalf("contract %s resolved to symbol=%q file=%q", id, items[0].SymbolID, items[0].FilePath)
		}
		if _, ok := items[0].Meta["handler_ident"]; ok {
			t.Fatalf("contract %s retained internal handler hint", id)
		}
	}
}

func TestExtractGoModContractsUsesOneExistingIDBatch(t *testing.T) {
	root := t.TempDir()
	goMod := []byte("module example.com/app\n\ngo 1.22\n\nrequire example.com/dependency v1.2.3\n")
	if err := os.WriteFile(filepath.Join(root, "go.mod"), goMod, 0o644); err != nil {
		t.Fatal(err)
	}
	base := graph.New()
	counting := &residualBatchCountingStore{Store: base}
	idx := &Indexer{graph: counting, rootPath: root, repoPrefix: "repo"}

	idx.extractGoModContracts(contracts.NewRegistry())

	if counting.getNodesByIDsCalls != 1 || counting.getNodeCalls != 0 {
		t.Fatalf("go.mod existing-node lookups: batch=%d point=%d", counting.getNodesByIDsCalls, counting.getNodeCalls)
	}
	if node := base.GetNode("dep::example.com/dependency"); node == nil || node.Kind != graph.KindContract {
		t.Fatalf("dependency contract node missing: %#v", node)
	}
}

func TestAffectedSetUsesBatchedFileAndMethodAdjacency(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "a:file", Kind: graph.KindFile, FilePath: "a.go", Language: "go"},
		{ID: "a:type", Kind: graph.KindType, Name: "A", FilePath: "a.go", Language: "go"},
		{ID: "a:method", Kind: graph.KindMethod, Name: "M", FilePath: "a.go", Language: "go"},
		{ID: "b:file", Kind: graph.KindFile, FilePath: "b.go", Language: "go"},
		{ID: "b:iface", Kind: graph.KindInterface, Name: "I", FilePath: "b.go", Language: "go"},
	}, []*graph.Edge{{From: "a:method", To: "a:type", Kind: graph.EdgeMemberOf, FilePath: "a.go"}})
	counting := &residualBatchCountingStore{Store: base}
	idx := &Indexer{graph: counting}

	types, ifaces := idx.affectedTypeSet([]string{"a.go", "b.go"})
	if !types["a:type"] || !types["b:iface"] || !ifaces["b:iface"] {
		t.Fatalf("affected set lost nodes: types=%v ifaces=%v", types, ifaces)
	}
	if counting.getFileNodesByPathsCalls != 1 || counting.getFileNodesCalls != 0 {
		t.Fatalf("affected file lookups: batch=%d point=%d", counting.getFileNodesByPathsCalls, counting.getFileNodesCalls)
	}
	if counting.getOutEdgesByIDsCalls != 1 || counting.getOutEdgesCalls != 0 {
		t.Fatalf("affected method adjacency: batch=%d point=%d", counting.getOutEdgesByIDsCalls, counting.getOutEdgesCalls)
	}

	counting.getFileNodesByPathsCalls = 0
	counting.getFileNodesCalls = 0
	root := t.TempDir()
	idx.rootPath = root
	if !idx.staleFilesAffectDerivedEdges([]string{filepath.Join(root, "a.go"), filepath.Join(root, "b.go")}) {
		t.Fatal("structural stale files were not detected")
	}
	if counting.getFileNodesByPathsCalls != 1 || counting.getFileNodesCalls != 0 {
		t.Fatalf("stale-file lookups: batch=%d point=%d", counting.getFileNodesByPathsCalls, counting.getFileNodesCalls)
	}
}

func TestRefreshContractsForFilesPrefetchesOneFrontier(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.go", "b.go"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("package sample\n\nfunc F() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "a:file", Kind: graph.KindFile, Name: "a.go", FilePath: "a.go", Language: "go"},
		{ID: "a:func", Kind: graph.KindFunction, Name: "F", FilePath: "a.go", StartLine: 3, EndLine: 3, Language: "go"},
		{ID: "b:file", Kind: graph.KindFile, Name: "b.go", FilePath: "b.go", Language: "go"},
		{ID: "b:func", Kind: graph.KindFunction, Name: "F", FilePath: "b.go", StartLine: 3, EndLine: 3, Language: "go"},
	}, nil)
	counting := &residualBatchCountingStore{Store: base}
	idx := newTestIndexer(counting)
	idx.rootPath = root
	idx.logger = zap.NewNop()
	idx.contractRegistry = contracts.NewRegistry()
	idx.contractCache = make(map[string]*contractCacheEntry)

	refresh := idx.refreshContractsForFiles([]string{"b.go", "a.go", "a.go"})
	if refresh.Changed || refresh.LegacyFallback {
		t.Fatalf("empty contract frontier changed=%v fallback=%v", refresh.Changed, refresh.LegacyFallback)
	}
	if counting.getFileNodesByPathsCalls != 1 || counting.getFileNodesCalls != 0 {
		t.Fatalf("contract file lookups: batch=%d point=%d", counting.getFileNodesByPathsCalls, counting.getFileNodesCalls)
	}
	if counting.getOutEdgesByIDsCalls != 1 || counting.getOutEdgesCalls != 0 {
		t.Fatalf("contract adjacency: batch=%d point=%d", counting.getOutEdgesByIDsCalls, counting.getOutEdgesCalls)
	}
	if len(idx.contractCache) != 2 {
		t.Fatalf("contract cache entries=%d, want 2", len(idx.contractCache))
	}
}

func TestSetReparsePendingEnrichmentsUsesOneReadAndWriteBatch(t *testing.T) {
	base := graph.New()
	base.AddBatch([]*graph.Node{
		{ID: "a:file", Kind: graph.KindFile, FilePath: "a.go", Meta: map[string]any{}},
		{ID: "b:file", Kind: graph.KindFile, FilePath: "b.go", Meta: map[string]any{graph.MetaReparsePendingEnrichment: true}},
	}, nil)
	counting := &residualBatchCountingStore{Store: base}
	idx := &Indexer{graph: counting}

	idx.setReparsePendingEnrichments(map[string]bool{"b.go": false, "a.go": true, "missing.go": true})

	if counting.getFileNodesByPathsCalls != 1 || counting.getFileNodesCalls != 0 {
		t.Fatalf("marker reads: batch=%d point=%d", counting.getFileNodesByPathsCalls, counting.getFileNodesCalls)
	}
	if counting.addBatchCalls != 1 || counting.addNodeCalls != 0 {
		t.Fatalf("marker writes: batch=%d point=%d", counting.addBatchCalls, counting.addNodeCalls)
	}
	if _, ok := base.GetNode("a:file").Meta[graph.MetaReparsePendingEnrichment]; !ok {
		t.Fatal("pending marker was not set")
	}
	if _, ok := base.GetNode("b:file").Meta[graph.MetaReparsePendingEnrichment]; ok {
		t.Fatal("pending marker was not cleared")
	}
}
