package store_sqlite

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func openMutationReceiptStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "mutation-receipt.sqlite"))
	if err != nil {
		t.Fatalf("open SQLite store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close SQLite store: %v", err)
		}
	})
	return store
}

func TestSQLiteMutationReceiptCapturesExactResolutionDelta(t *testing.T) {
	store := openMutationReceiptStore(t)
	token := store.BeginMutationReceipt()
	store.AddBatch([]*graph.Node{
		{ID: "repo/src/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", QualName: "pkg.Caller", FilePath: "src/a.go", RepoPrefix: "repo"},
		{ID: "repo/src/b.go::Load", Kind: graph.KindFunction, Name: "Load", QualName: "pkg.Load", FilePath: "src/b.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{
		From: "repo/src/a.go::Caller", To: "repo::" + graph.UnresolvedMarker + "Load", Kind: graph.EdgeImports,
		FilePath: "src/a.go", Alias: "loader",
	}})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("receipt = %+v, want complete resolution delta", receipt)
	}
	if want := []string{"src/a.go", "src/b.go"}; !slices.Equal(receipt.ResolutionFiles(), want) {
		t.Fatalf("resolution files = %v, want %v", receipt.ResolutionFiles(), want)
	}
	assertSQLiteReceiptContains(t, "target names", receipt.TargetNames, "Caller", "Load", "pkg.Caller", "pkg.Load")
	assertSQLiteReceiptContains(t, "target ids", receipt.TargetIDs,
		"repo/src/a.go::Caller", "repo/src/b.go::Load", "repo::"+graph.UnresolvedMarker+"Load")
	assertSQLiteReceiptContains(t, "import candidates", receipt.ImportCandidates, "Load", "loader")
}

func TestSQLiteMutationReceiptIdempotentAndAttributeOnlyWritesAreNeutral(t *testing.T) {
	store := openMutationReceiptStore(t)
	node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"}
	edge := &graph.Edge{From: node.ID, To: "repo::" + graph.UnresolvedMarker + "B", Kind: graph.EdgeCalls, FilePath: "a.go"}
	store.AddNode(node)
	store.AddEdge(edge)

	token := store.BeginMutationReceipt()
	store.AddNode(node)
	store.AddEdge(edge)
	store.PersistEdgeAttributes(&graph.Edge{
		From: edge.From, To: edge.To, Kind: edge.Kind, FilePath: edge.FilePath,
		Confidence: 0.9, ConfidenceLabel: "HIGH", Origin: "semantic", Tier: "semantic",
	})
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete {
		t.Fatalf("neutral receipt unexpectedly incomplete: %+v", receipt)
	}
	if receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("neutral writes produced resolution delta: %+v", receipt)
	}
}

func TestSQLiteMutationReceiptIdentityChangingUpsertFailsClosed(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"})

	token := store.BeginMutationReceipt()
	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "Renamed", QualName: "pkg.Renamed", FilePath: "a.go", RepoPrefix: "repo"})
	receipt := store.EndMutationReceipt(token)
	if receipt.Complete {
		t.Fatalf("identity-changing UPSERT returned complete receipt: %+v", receipt)
	}
	if receipt.IncompleteReason != "node_identity_changed" {
		t.Fatalf("incomplete reason = %q, want node_identity_changed", receipt.IncompleteReason)
	}
}

func TestSQLiteMutationReceiptDuplicateNodeBatchNamesIncompleteReason(t *testing.T) {
	store := openMutationReceiptStore(t)
	const id = "repo/a.go::A"
	store.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: "A", QualName: "pkg.A", FilePath: "a.go", RepoPrefix: "repo"})

	token := store.BeginMutationReceipt()
	store.AddBatch([]*graph.Node{
		{ID: id, Kind: graph.KindFunction, Name: "FirstRename", QualName: "pkg.FirstRename", FilePath: "a.go", RepoPrefix: "repo"},
		{ID: id, Kind: graph.KindFunction, Name: "FinalRename", QualName: "pkg.FinalRename", FilePath: "a.go", RepoPrefix: "repo"},
	}, nil)
	receipt := store.EndMutationReceipt(token)
	if receipt.Complete {
		t.Fatalf("identity-changing duplicate batch returned complete receipt: %+v", receipt)
	}
	if receipt.IncompleteReason != "duplicate_node_batch" {
		t.Fatalf("incomplete reason = %q, want duplicate_node_batch", receipt.IncompleteReason)
	}
}

func TestSQLiteMutationReceiptBatchRollbackPublishesNothing(t *testing.T) {
	store := openMutationReceiptStore(t)
	token := store.BeginMutationReceipt()
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("invalid batch unexpectedly succeeded")
			}
		}()
		store.AddBatch([]*graph.Node{
			{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"},
			{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "b.go", RepoPrefix: "repo", Meta: map[string]any{"unsupported": make(chan int)}},
		}, nil)
	}()
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("rolled-back batch leaked receipt events: %+v", receipt)
	}
	if node := store.GetNode("repo/a.go::A"); node != nil {
		t.Fatalf("rolled-back batch leaked node: %+v", node)
	}
}

func TestSQLiteMutationReceiptsOverlapWithoutStealingEvents(t *testing.T) {
	store := openMutationReceiptStore(t)
	outer := store.BeginMutationReceipt()
	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
	inner := store.BeginMutationReceipt()
	store.AddNode(&graph.Node{ID: "repo/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "b.go", RepoPrefix: "repo"})
	outerReceipt := store.EndMutationReceipt(outer)
	store.AddNode(&graph.Node{ID: "repo/c.go::C", Kind: graph.KindFunction, Name: "C", FilePath: "c.go", RepoPrefix: "repo"})
	innerReceipt := store.EndMutationReceipt(inner)

	assertSQLiteReceiptContains(t, "outer files", outerReceipt.ResolutionFiles(), "a.go", "b.go")
	if slices.Contains(outerReceipt.ResolutionFiles(), "c.go") {
		t.Fatalf("outer receipt observed mutation after it ended: %+v", outerReceipt)
	}
	assertSQLiteReceiptContains(t, "inner files", innerReceipt.ResolutionFiles(), "b.go", "c.go")
	if slices.Contains(innerReceipt.ResolutionFiles(), "a.go") {
		t.Fatalf("inner receipt observed mutation before it began: %+v", innerReceipt)
	}
}

func TestSQLiteMutationReceiptsConcurrentOverlap(t *testing.T) {
	store := openMutationReceiptStore(t)
	const workers = 12
	ready := sync.WaitGroup{}
	ready.Add(workers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			token := store.BeginMutationReceipt()
			ready.Done()
			<-start
			id := string(rune('a' + i))
			store.AddNode(&graph.Node{ID: "repo/" + id + ".go::" + id, Kind: graph.KindFunction, Name: id, FilePath: id + ".go", RepoPrefix: "repo"})
			receipt := store.EndMutationReceipt(token)
			if !receipt.Complete || !receipt.ResolutionRelevant || !slices.Contains(receipt.DefinitionFiles, id+".go") {
				t.Errorf("concurrent receipt %d = %+v", i, receipt)
			}
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
}

func TestSQLiteMutationReceiptBoundaryWaitsForInFlightWrite(t *testing.T) {
	store := openMutationReceiptStore(t)
	token := store.BeginMutationReceipt()

	store.writeMu.Lock()
	started := make(chan struct{})
	receiptCh := make(chan graph.MutationReceipt, 1)
	go func() {
		close(started)
		receiptCh <- store.EndMutationReceipt(token)
	}()
	<-started
	select {
	case receipt := <-receiptCh:
		store.writeMu.Unlock()
		t.Fatalf("EndMutationReceipt overtook in-flight write: %+v", receipt)
	case <-time.After(20 * time.Millisecond):
	}

	node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"}
	changed, err := store.insertNodeLocked(store.stmtInsertNode, node)
	if err != nil {
		store.writeMu.Unlock()
		t.Fatalf("insert node: %v", err)
	}
	delta := newSQLiteMutationReceiptAccumulator()
	recordSQLiteAddedNode(delta, node)
	if changed {
		store.mergeMutationReceiptLocked(delta)
	}
	store.writeMu.Unlock()

	select {
	case receipt := <-receiptCh:
		if !receipt.Complete || !receipt.ResolutionRelevant || !slices.Contains(receipt.DefinitionFiles, "a.go") {
			t.Fatalf("receipt missed in-flight write: %+v", receipt)
		}
	case <-time.After(time.Second):
		t.Fatal("EndMutationReceipt did not complete after write drained")
	}
}

func TestSQLiteMutationReceiptEdgeSourceFileFallback(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})

	token := store.BeginMutationReceipt()
	store.AddEdge(&graph.Edge{From: "repo/a.go::A", To: "repo::" + graph.UnresolvedMarker + "B", Kind: graph.EdgeCalls})
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || !receipt.ResolutionRelevant || !slices.Contains(receipt.ChangedFiles, "a.go") {
		t.Fatalf("source-file fallback receipt = %+v", receipt)
	}

	missing := store.BeginMutationReceipt()
	store.AddEdge(&graph.Edge{From: "repo/missing.go::Missing", To: "repo::" + graph.UnresolvedMarker + "C", Kind: graph.EdgeCalls})
	if receipt := store.EndMutationReceipt(missing); receipt.Complete {
		t.Fatalf("missing source file returned complete receipt: %+v", receipt)
	}
}

func TestSQLiteMutationReceiptNoOpMutationsStayComplete(t *testing.T) {
	store := openMutationReceiptStore(t)
	token := store.BeginMutationReceipt()
	if store.RemoveEdge("missing", "missing", graph.EdgeCalls) {
		t.Fatal("missing edge unexpectedly removed")
	}
	if err := store.PurgeRepo("missing"); err != nil {
		t.Fatalf("purge missing repo: %v", err)
	}
	if err := store.RekeyRepoPrefix("missing", "new"); err != nil {
		t.Fatalf("rekey missing repo: %v", err)
	}
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("no-op mutations changed receipt: %+v", receipt)
	}
}

func TestSQLiteMutationReceiptPurgeAndRekeyFailClosedAfterChange(t *testing.T) {
	t.Run("purge", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
		token := store.BeginMutationReceipt()
		if err := store.PurgeRepo("repo"); err != nil {
			t.Fatalf("purge repo: %v", err)
		}
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("purge returned complete receipt: %+v", receipt)
		}
	})

	t.Run("purge sidecar only", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		if err := store.SetFileMtime("repo", "a.go", 1); err != nil {
			t.Fatalf("seed file mtime: %v", err)
		}
		token := store.BeginMutationReceipt()
		if err := store.PurgeRepo("repo"); err != nil {
			t.Fatalf("purge repo: %v", err)
		}
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("sidecar-only purge returned complete receipt: %+v", receipt)
		}
	})

	t.Run("rekey", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		if err := store.SetFileMtime("old", "a.go", 1); err != nil {
			t.Fatalf("seed file mtime: %v", err)
		}
		token := store.BeginMutationReceipt()
		if err := store.RekeyRepoPrefix("old", "new"); err != nil {
			t.Fatalf("rekey repo: %v", err)
		}
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("rekey returned complete receipt: %+v", receipt)
		}
	})
}

func TestSQLiteMutationReceiptBulkBoundariesFailClosed(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		token := store.BeginMutationReceipt()
		store.BeginBulkLoad()
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("BeginBulkLoad returned complete receipt: %+v", receipt)
		}
		if err := store.FlushBulk(); err != nil {
			t.Fatalf("flush bulk: %v", err)
		}
	})

	t.Run("flush", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		store.BeginBulkLoad()
		token := store.BeginMutationReceipt()
		if err := store.FlushBulk(); err != nil {
			t.Fatalf("flush bulk: %v", err)
		}
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("FlushBulk returned complete receipt: %+v", receipt)
		}
	})
}

func TestSQLiteMutationReceiptTopologyMutations(t *testing.T) {
	t.Run("reindex edge", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		edge := &graph.Edge{From: "repo/a.go::A", To: "repo/old.go::Old", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
		store.AddEdge(edge)
		updated := *edge
		updated.To = "repo/new.go::New"
		token := store.BeginMutationReceipt()
		store.ReindexEdge(&updated, edge.To)
		if receipt := store.EndMutationReceipt(token); !receipt.Complete || receipt.ResolutionRelevant {
			t.Fatalf("ReindexEdge receipt = %+v, want complete and resolution-irrelevant", receipt)
		}

		noop := store.BeginMutationReceipt()
		store.ReindexEdge(&updated, updated.To)
		if receipt := store.EndMutationReceipt(noop); !receipt.Complete {
			t.Fatalf("no-op ReindexEdge returned incomplete receipt: %+v", receipt)
		}
	})

	t.Run("reindex edges", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		edge := &graph.Edge{From: "repo/a.go::A", To: "repo/old.go::Old", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
		store.AddEdge(edge)
		updated := *edge
		updated.To = "repo/new.go::New"
		token := store.BeginMutationReceipt()
		store.ReindexEdges([]graph.EdgeReindex{{Edge: &updated, OldTo: edge.To}})
		if receipt := store.EndMutationReceipt(token); !receipt.Complete || receipt.ResolutionRelevant {
			t.Fatalf("ReindexEdges receipt = %+v, want complete and resolution-irrelevant", receipt)
		}

		noop := store.BeginMutationReceipt()
		store.ReindexEdges([]graph.EdgeReindex{{Edge: &updated, OldTo: updated.To}})
		if receipt := store.EndMutationReceipt(noop); !receipt.Complete {
			t.Fatalf("no-op ReindexEdges returned incomplete receipt: %+v", receipt)
		}
	})

	t.Run("evict file", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
		token := store.BeginMutationReceipt()
		nodes, _ := store.EvictFile("a.go")
		if nodes != 1 {
			t.Fatalf("EvictFile nodes = %d, want 1", nodes)
		}
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("EvictFile returned complete receipt: %+v", receipt)
		}

		noop := store.BeginMutationReceipt()
		store.EvictFile("missing.go")
		if receipt := store.EndMutationReceipt(noop); !receipt.Complete {
			t.Fatalf("no-op EvictFile returned incomplete receipt: %+v", receipt)
		}
	})

	t.Run("evict repo", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
		token := store.BeginMutationReceipt()
		nodes, _ := store.EvictRepo("repo")
		if nodes != 1 {
			t.Fatalf("EvictRepo nodes = %d, want 1", nodes)
		}
		if receipt := store.EndMutationReceipt(token); receipt.Complete {
			t.Fatalf("EvictRepo returned complete receipt: %+v", receipt)
		}

		noop := store.BeginMutationReceipt()
		store.EvictRepo("missing")
		if receipt := store.EndMutationReceipt(noop); !receipt.Complete {
			t.Fatalf("no-op EvictRepo returned incomplete receipt: %+v", receipt)
		}
	})
}

func TestSQLiteMutationReceiptReindexEdgeRollbackIsAtomic(t *testing.T) {
	store := openMutationReceiptStore(t)
	edge := &graph.Edge{From: "repo/a.go::A", To: "repo/old.go::Old", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
	store.AddEdge(edge)
	bad := *edge
	bad.To = "repo/new.go::New"
	bad.Meta = map[string]any{"unsupported": make(chan int)}

	token := store.BeginMutationReceipt()
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("invalid ReindexEdge unexpectedly succeeded")
			}
		}()
		store.ReindexEdge(&bad, edge.To)
	}()
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("rolled-back ReindexEdge changed receipt: %+v", receipt)
	}
	edges := store.GetOutEdges(edge.From)
	if len(edges) != 1 || edges[0].To != edge.To {
		t.Fatalf("rolled-back ReindexEdge changed stored topology: %+v", edges)
	}
}

func TestSQLiteMutationReceiptProvenanceOnlyWriteIsNeutral(t *testing.T) {
	store := openMutationReceiptStore(t)
	edge := &graph.Edge{
		From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls,
		FilePath: "a.go", Line: 1, Origin: "heuristic", Tier: "heuristic",
	}
	store.AddEdge(edge)
	token := store.BeginMutationReceipt()
	if !store.SetEdgeProvenance(edge, "semantic") {
		t.Fatal("SetEdgeProvenance did not update edge")
	}
	receipt := store.EndMutationReceipt(token)
	if !receipt.Complete || receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
		t.Fatalf("provenance-only write changed receipt: %+v", receipt)
	}
}

func TestSQLiteDuplicateEdgeWritesPreservePersistedAnalysis(t *testing.T) {
	t.Run("AddEdge", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		edge := &graph.Edge{From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
		store.AddEdge(edge)
		buildMinimalAnalysisGeneration(t, store, "add-edge-noop", 0, true)
		before := store.AnalysisMutationRevision()

		store.AddEdge(edge)
		if after := store.AnalysisMutationRevision(); after != before {
			t.Fatalf("duplicate AddEdge advanced analysis revision: before=%d after=%d", before, after)
		}
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
			t.Fatalf("load active analysis: %v", err)
		} else if !found {
			t.Fatal("duplicate AddEdge discarded active analysis")
		}
	})

	t.Run("AddBatch", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		node := &graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"}
		edge := &graph.Edge{From: node.ID, To: "repo/b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}
		store.AddBatch([]*graph.Node{node}, []*graph.Edge{edge})
		buildMinimalAnalysisGeneration(t, store, "add-batch-noop", 0, true)
		before := store.AnalysisMutationRevision()

		store.AddBatch([]*graph.Node{nil, node}, []*graph.Edge{nil, edge})
		if after := store.AnalysisMutationRevision(); after != before {
			t.Fatalf("duplicate AddBatch advanced analysis revision: before=%d after=%d", before, after)
		}
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
			t.Fatalf("load active analysis: %v", err)
		} else if !found {
			t.Fatal("duplicate AddBatch discarded active analysis")
		}
	})

	t.Run("new AddEdge invalidates", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		buildMinimalAnalysisGeneration(t, store, "add-edge-change", 0, true)
		store.AddEdge(&graph.Edge{From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1})
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
			t.Fatalf("load active analysis: %v", err)
		} else if found {
			t.Fatal("new AddEdge preserved stale active analysis")
		}
	})

	t.Run("new AddBatch edge invalidates", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		buildMinimalAnalysisGeneration(t, store, "add-batch-change", 0, true)
		store.AddBatch(nil, []*graph.Edge{{From: "repo/a.go::A", To: "repo/b.go::B", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1}})
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
			t.Fatalf("load active analysis: %v", err)
		} else if found {
			t.Fatal("new AddBatch edge preserved stale active analysis")
		}
	})

	t.Run("filtered batch", func(t *testing.T) {
		store := openMutationReceiptStore(t)
		buildMinimalAnalysisGeneration(t, store, "filtered-batch-noop", 0, true)
		store.AddBatch([]*graph.Node{nil}, []*graph.Edge{nil})
		if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
			t.Fatalf("load active analysis: %v", err)
		} else if !found {
			t.Fatal("filtered AddBatch discarded active analysis")
		}
	})
}

func TestSQLitePurgeInvalidatesPersistedAnalysis(t *testing.T) {
	store := openMutationReceiptStore(t)
	store.AddNode(&graph.Node{ID: "repo/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "a.go", RepoPrefix: "repo"})
	buildMinimalAnalysisGeneration(t, store, "purge", 0, true)
	before := store.AnalysisMutationRevision()

	if err := store.PurgeRepo("repo"); err != nil {
		t.Fatalf("purge repo: %v", err)
	}
	if after := store.AnalysisMutationRevision(); after <= before {
		t.Fatalf("analysis revision did not advance: before=%d after=%d", before, after)
	}
	if _, found, err := store.LoadActiveAnalysisHeader(77); err != nil {
		t.Fatalf("load active analysis: %v", err)
	} else if found {
		t.Fatal("purge left stale analysis generation active")
	}
}

func TestSQLiteMutationReceiptUnknownTokenFailsClosed(t *testing.T) {
	store := openMutationReceiptStore(t)
	token := store.BeginMutationReceipt()
	_ = store.EndMutationReceipt(token)
	if receipt := store.EndMutationReceipt(token); receipt.Complete {
		t.Fatalf("already-ended token returned complete receipt: %+v", receipt)
	}
}

func assertSQLiteReceiptContains(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	for _, value := range want {
		if !slices.Contains(got, value) {
			t.Errorf("%s = %v, missing %q", label, got, value)
		}
	}
}
