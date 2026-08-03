package store_sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestUnresolvedEdgePagerHonorsCancellation(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "unresolved-cancel.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddEdge(&graph.Edge{From: "caller", To: "unresolved::target", Kind: graph.EdgeCalls})

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := store.BeginUnresolvedEdgeScan(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("BeginUnresolvedEdgeScan error = %v, want context.Canceled", err)
	}

	scan, err := store.BeginUnresolvedEdgeScan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReadUnresolvedEdgePage(canceled, scan, 0, 16, 1<<20); !errors.Is(err, context.Canceled) {
		t.Fatalf("ReadUnresolvedEdgePage error = %v, want context.Canceled", err)
	}
}

func TestUnresolvedEdgePagesStableHighWaterAndBounds(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "unresolved.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	var nodes []*graph.Node
	var edges []*graph.Edge
	for i := 0; i < 7; i++ {
		id := fmt.Sprintf("from-%d", i)
		nodes = append(nodes, &graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
	}
	for i := 0; i < 5; i++ {
		edges = append(edges, &graph.Edge{
			From: fmt.Sprintf("from-%d", i), To: fmt.Sprintf("unresolved::target-%d", i),
			Kind: graph.EdgeCalls, FilePath: "x.go", Line: i + 1,
		})
	}
	store.AddBatch(nodes, edges)

	scan, err := store.BeginUnresolvedEdgeScan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if scan.PendingBefore != -1 {
		t.Fatalf("PendingBefore = %d, want -1 (not pre-counted)", scan.PendingBefore)
	}
	for label, plan := range map[string]string{
		"high_water": queryPlan(t, store, `SELECT id FROM edges INDEXED BY edges_by_unresolved WHERE `+unresolvedEdgePredicate+` ORDER BY id DESC LIMIT 1`),
		"page":       queryPlan(t, store, `SELECT id FROM edges WHERE id > ? AND id <= ? AND `+unresolvedEdgePredicate+` ORDER BY id LIMIT ?`, 0, scan.HighWaterID, 2),
	} {
		plan = strings.ToLower(plan)
		if !strings.Contains(plan, "edges_by_unresolved") || strings.Contains(plan, "scan edges") {
			t.Fatalf("%s unresolved plan is not a bounded index search:\n%s", label, plan)
		}
	}
	first, err := store.ReadUnresolvedEdgePage(t.Context(), scan, 0, 2, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Edges) != 2 || first.Exhausted {
		t.Fatalf("first page = %d edges, exhausted=%v; want 2,false", len(first.Edges), first.Exhausted)
	}

	// Both mutations occur after the scan boundary. The late row and the
	// replacement row created by reindex must not leak back into this pass.
	store.AddEdge(&graph.Edge{From: "from-6", To: "unresolved::late", Kind: graph.EdgeCalls, FilePath: "x.go", Line: 7})
	resolved := first.Edges[0]
	oldTo := resolved.To
	resolved.To = "from-5"
	store.ReindexEdges([]graph.EdgeReindex{{Edge: resolved, OldTo: oldTo}})

	seen := make(map[string]int)
	for _, edge := range first.Edges {
		seen[edge.From]++
	}
	after := first.NextID
	for !first.Exhausted {
		page, err := store.ReadUnresolvedEdgePage(t.Context(), scan, after, 2, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Edges) > 2 {
			t.Fatalf("page retained %d rows, want <= 2", len(page.Edges))
		}
		for _, edge := range page.Edges {
			seen[edge.From]++
		}
		if page.NextID < after {
			t.Fatalf("cursor regressed from %d to %d", after, page.NextID)
		}
		after, first.Exhausted = page.NextID, page.Exhausted
	}
	if seen["from-6"] != 0 {
		t.Fatal("row inserted above high water leaked into the scan")
	}
	for i := 0; i < 5; i++ {
		if got := seen[fmt.Sprintf("from-%d", i)]; got != 1 {
			t.Fatalf("original source %d visited %d times, want once", i, got)
		}
	}
}

func TestPartialUnresolvedIndexTracksTargetTransitions(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "unresolved-transitions.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "a"},
		{ID: "b", Kind: graph.KindFunction, Name: "b"},
		{ID: "c", Kind: graph.KindFunction, Name: "c"},
	}, []*graph.Edge{
		{From: "a", To: "unresolved::b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1},
		{From: "c", To: "b", Kind: graph.EdgeCalls, FilePath: "c.go", Line: 1},
	})

	indexCount := func() int {
		t.Helper()
		var count int
		if err := store.db.QueryRow(`SELECT COUNT(*) FROM edges INDEXED BY edges_by_unresolved WHERE is_unresolved = 1`).Scan(&count); err != nil {
			t.Fatalf("count partial unresolved index: %v", err)
		}
		return count
	}
	if got := indexCount(); got != 1 {
		t.Fatalf("initial unresolved index rows=%d, want 1", got)
	}

	moving := store.GetOutEdges("a")[0]
	oldTo := moving.To
	moving.To = "b"
	store.ReindexEdges([]graph.EdgeReindex{{Edge: moving, OldTo: oldTo}})
	if got := indexCount(); got != 0 {
		t.Fatalf("resolved transition left %d partial-index rows, want 0", got)
	}
	resolvedScan, err := store.BeginUnresolvedEdgeScan(t.Context())
	if err != nil || resolvedScan.PendingBefore != 0 {
		t.Fatalf("resolved scan pending=%d err=%v, want 0,nil", resolvedScan.PendingBefore, err)
	}
	resolvedFrontier, err := store.CountUnresolvedFrontier()
	if err != nil || resolvedFrontier.Pending != 0 {
		t.Fatalf("resolved frontier pending=%d err=%v, want 0,nil", resolvedFrontier.Pending, err)
	}

	moving = store.GetOutEdges("a")[0]
	oldTo = moving.To
	moving.To = "repo::unresolved::again"
	store.ReindexEdges([]graph.EdgeReindex{{Edge: moving, OldTo: oldTo}})
	if got := indexCount(); got != 1 {
		t.Fatalf("unresolved transition produced %d partial-index rows, want 1", got)
	}
	unresolvedScan, err := store.BeginUnresolvedEdgeScan(t.Context())
	if err != nil || unresolvedScan.PendingBefore != -1 {
		t.Fatalf("unresolved scan pending=%d err=%v, want -1,nil", unresolvedScan.PendingBefore, err)
	}
	page, err := store.ReadUnresolvedEdgePage(t.Context(), unresolvedScan, 0, 10, 1<<20)
	if err != nil || len(page.Edges) != 1 || page.Edges[0].To != "repo::unresolved::again" || !page.Exhausted {
		t.Fatalf("unresolved page=%+v err=%v, want one terminal transitioned edge", page, err)
	}
	frontier, err := store.CountUnresolvedFrontier()
	if err != nil || frontier.Pending != 1 {
		t.Fatalf("unresolved frontier pending=%d err=%v, want 1,nil", frontier.Pending, err)
	}
}

func TestUnresolvedEdgePageByteBoundMakesProgress(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "unresolved-bytes.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindFunction, Name: "a"},
		{ID: "b", Kind: graph.KindFunction, Name: "b"},
	}, []*graph.Edge{
		{From: "a", To: "unresolved::a", Kind: graph.EdgeCalls, Meta: map[string]any{"large": string(make([]byte, 32<<10))}},
		{From: "b", To: "unresolved::b", Kind: graph.EdgeCalls},
	})

	scan, err := store.BeginUnresolvedEdgeScan(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ReadUnresolvedEdgePage(t.Context(), scan, 0, 100, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Edges) != 1 || page.NextID == 0 || page.Exhausted {
		t.Fatalf("oversized page = len %d next %d exhausted %v; want one advancing non-terminal row", len(page.Edges), page.NextID, page.Exhausted)
	}
	next, err := store.ReadUnresolvedEdgePage(t.Context(), scan, page.NextID, 100, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Edges) != 1 || !next.Exhausted {
		t.Fatalf("second page = len %d exhausted %v; want one terminal row", len(next.Edges), next.Exhausted)
	}
}

func TestUnresolvedIteratorHonorsEarlyStop(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "unresolved-stop.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	edges := make([]*graph.Edge, 0, 5000)
	for i := 0; i < 5000; i++ {
		edges = append(edges, &graph.Edge{From: fmt.Sprintf("f-%d", i), To: fmt.Sprintf("unresolved::t-%d", i), Kind: graph.EdgeCalls})
	}
	store.AddBatch(nil, edges)
	visited := 0
	store.EdgesWithUnresolvedTarget()(func(*graph.Edge) bool {
		visited++
		return false
	})
	if visited != 1 {
		t.Fatalf("early-stop iterator visited %d rows, want 1", visited)
	}
}
