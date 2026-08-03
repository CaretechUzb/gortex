package store_sqlite

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func openReindexReceiptTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "receipt.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedReindexReceiptEdge(t *testing.T, store *Store, target, file string) *graph.Edge {
	t.Helper()
	store.AddNode(&graph.Node{
		ID:       "caller",
		Kind:     graph.KindFunction,
		Name:     "caller",
		FilePath: "repo/caller.go",
	})
	edge := &graph.Edge{
		From:     "caller",
		To:       target,
		Kind:     graph.EdgeCalls,
		FilePath: file,
		Line:     17,
	}
	store.AddEdge(edge)
	return edge
}

func TestSQLiteReindexResolvedTargetKeepsReceiptExact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reindex func(*Store, *graph.Edge, string)
	}{
		{
			name: "single",
			reindex: func(store *Store, edge *graph.Edge, oldTo string) {
				store.ReindexEdge(edge, oldTo)
			},
		},
		{
			name: "batch",
			reindex: func(store *Store, edge *graph.Edge, oldTo string) {
				store.ReindexEdges([]graph.EdgeReindex{{Edge: edge, OldTo: oldTo}})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openReindexReceiptTestStore(t)
			edge := seedReindexReceiptEdge(t, store, "unresolved::Target", "repo/caller.go")

			token := store.BeginMutationReceipt()
			oldTo := edge.To
			edge.To = "repo/target.go::Target"
			tc.reindex(store, edge, oldTo)
			receipt := store.EndMutationReceipt(token)

			if !receipt.Complete {
				t.Fatal("resolved reindex made exact receipt incomplete")
			}
			if receipt.ResolutionRelevant || len(receipt.ResolutionFiles()) != 0 {
				t.Fatalf("resolved reindex created catch-up work: %#v", receipt)
			}
			incoming := store.GetInEdges(edge.To)
			if len(incoming) != 1 || incoming[0].From != edge.From {
				t.Fatalf("resolved topology not persisted: %#v", incoming)
			}
		})
	}
}

func TestSQLiteReindexUnresolvedTargetRecordsExactFrontier(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	edge := seedReindexReceiptEdge(t, store, "unresolved::Old", "")

	token := store.BeginMutationReceipt()
	oldTo := edge.To
	edge.To = "unresolved::New"
	store.ReindexEdges([]graph.EdgeReindex{{Edge: edge, OldTo: oldTo}})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("unresolved reindex receipt = %#v, want complete and relevant", receipt)
	}
	if want := []string{"repo/caller.go"}; !reflect.DeepEqual(receipt.ChangedFiles, want) {
		t.Fatalf("changed files = %v, want %v", receipt.ChangedFiles, want)
	}
	if want := []string{"New"}; !reflect.DeepEqual(receipt.TargetNames, want) {
		t.Fatalf("target names = %v, want %v", receipt.TargetNames, want)
	}
	incoming := store.GetInEdges("unresolved::New")
	if len(incoming) != 1 || incoming[0].From != edge.From {
		t.Fatalf("unresolved topology not persisted: %#v", incoming)
	}
}

func TestSQLiteReindexMissingSourceFailsClosedOnlyForNewUnresolvedEdge(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	edge := &graph.Edge{
		From: "missing-caller",
		To:   "unresolved::Old",
		Kind: graph.EdgeCalls,
		Line: 9,
	}
	store.AddEdge(edge)

	token := store.BeginMutationReceipt()
	oldTo := edge.To
	edge.To = "unresolved::New"
	store.ReindexEdges([]graph.EdgeReindex{{Edge: edge, OldTo: oldTo}})
	receipt := store.EndMutationReceipt(token)

	if receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("missing-source receipt = %#v, want incomplete and relevant", receipt)
	}
}

func TestSQLiteReindexDuplicateDestinationCreatesNoCatchupWork(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	oldEdge := seedReindexReceiptEdge(t, store, "unresolved::Old", "repo/caller.go")
	store.AddEdge(&graph.Edge{
		From:     oldEdge.From,
		To:       "unresolved::Existing",
		Kind:     oldEdge.Kind,
		FilePath: oldEdge.FilePath,
		Line:     oldEdge.Line,
	})

	token := store.BeginMutationReceipt()
	oldTo := oldEdge.To
	oldEdge.To = "unresolved::Existing"
	store.ReindexEdges([]graph.EdgeReindex{{Edge: oldEdge, OldTo: oldTo}})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete || receipt.ResolutionRelevant {
		t.Fatalf("duplicate destination receipt = %#v, want complete and irrelevant", receipt)
	}
	incoming := store.GetInEdges("unresolved::Existing")
	if len(incoming) != 1 {
		t.Fatalf("duplicate destination rows = %d, want 1", len(incoming))
	}
}

func TestSQLiteRefreshUnresolvedIdentityRecordsNewFile(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	edge := seedReindexReceiptEdge(t, store, "unresolved::Target", "repo/old.go")

	token := store.BeginMutationReceipt()
	edge.FilePath = "repo/new.go"
	edge.Line = 23
	store.ReindexEdges([]graph.EdgeReindex{{
		Edge:            edge,
		OldTo:           edge.To,
		RefreshIdentity: true,
		OldFilePath:     "repo/old.go",
		OldLine:         17,
	}})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("identity refresh receipt = %#v, want complete and relevant", receipt)
	}
	if want := []string{"repo/new.go"}; !reflect.DeepEqual(receipt.ChangedFiles, want) {
		t.Fatalf("changed files = %v, want %v", receipt.ChangedFiles, want)
	}
}

func TestSQLiteReceiptCapturesResolvedEdgeAndIgnoresNonDefinitionNoise(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "caller", Kind: graph.KindFunction, Name: "caller", FilePath: "repo/caller.go"},
		{ID: "resolved", Kind: graph.KindFunction, Name: "resolved", FilePath: "repo/resolved.go"},
	}, nil)

	token := store.BeginMutationReceipt()
	store.AddBatch([]*graph.Node{
		{ID: "file", Kind: graph.KindFile, Name: "noise.go", FilePath: "repo/noise.go"},
	}, []*graph.Edge{
		{From: "caller", To: "resolved", Kind: graph.EdgeCalls, FilePath: "repo/noise.go", Line: 1},
		{From: "caller", To: "unresolved::Needed", Kind: graph.EdgeCalls, FilePath: "repo/needed.go", Line: 2},
	})
	receipt := store.EndMutationReceipt(token)

	if !receipt.Complete || !receipt.ResolutionRelevant {
		t.Fatalf("mixed receipt = %#v, want complete and relevant", receipt)
	}
	if want := []string{"repo/needed.go", "repo/noise.go"}; !reflect.DeepEqual(receipt.ResolutionFiles(), want) {
		t.Fatalf("resolution files = %v, want %v", receipt.ResolutionFiles(), want)
	}
}
