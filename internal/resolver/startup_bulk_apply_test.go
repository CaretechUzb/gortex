package resolver

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const startupBulkApplyFixtureEdges = 4096

type startupBulkApplyPagerStore struct {
	*graph.Graph
	pending           []*graph.Edge
	reindexBatchSizes []int
}

func (s *startupBulkApplyPagerStore) BeginUnresolvedEdgeScan(context.Context) (graph.UnresolvedEdgeScan, error) {
	return graph.UnresolvedEdgeScan{
		HighWaterID:   int64(len(s.pending)),
		PendingBefore: len(s.pending),
	}, nil
}

func (s *startupBulkApplyPagerStore) ReadUnresolvedEdgePage(
	_ context.Context,
	_ graph.UnresolvedEdgeScan,
	afterID int64,
	maxRows, _ int,
) (graph.UnresolvedEdgePage, error) {
	start := int(afterID)
	if start >= len(s.pending) {
		return graph.UnresolvedEdgePage{NextID: afterID, Exhausted: true}, nil
	}
	end := start + maxRows
	if end > len(s.pending) {
		end = len(s.pending)
	}
	return graph.UnresolvedEdgePage{
		Edges:     s.pending[start:end],
		NextID:    int64(end),
		Exhausted: end == len(s.pending),
	}, nil
}

func (s *startupBulkApplyPagerStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexBatchSizes = append(s.reindexBatchSizes, len(batch))
	s.Graph.ReindexEdges(batch)
}

func runStartupBulkApplyFixture(t *testing.T, startup bool) (*ResolveStats, []int, []string) {
	t.Helper()

	base := graph.New()
	source := &graph.Node{
		ID:         "repo/src.go::caller",
		Kind:       graph.KindFunction,
		Name:       "caller",
		FilePath:   "repo/src.go",
		Language:   "go",
		RepoPrefix: "repo",
	}
	target := &graph.Node{
		ID:         "repo/src.go::foo",
		Kind:       graph.KindFunction,
		Name:       "foo",
		FilePath:   "repo/src.go",
		Language:   "go",
		RepoPrefix: "repo",
	}
	base.AddNode(source)
	base.AddNode(target)

	pending := make([]*graph.Edge, 0, startupBulkApplyFixtureEdges)
	for i := 0; i < startupBulkApplyFixtureEdges; i++ {
		edge := &graph.Edge{
			From:     source.ID,
			To:       "unresolved::foo",
			Kind:     graph.EdgeCalls,
			FilePath: source.FilePath,
			Line:     i + 1,
			Origin:   "ast",
		}
		base.AddEdge(edge)
		pending = append(pending, edge)
	}

	store := &startupBulkApplyPagerStore{Graph: base, pending: pending}
	resolver := New(store)
	resolver.SetStartupBulkApply(startup)
	stats := resolver.ResolveAll()

	targets := make([]string, len(pending))
	for i, edge := range pending {
		targets[i] = edge.To
		if edge.To != target.ID {
			t.Fatalf("edge %d target = %q, want %q", i, edge.To, target.ID)
		}
	}
	return stats, append([]int(nil), store.reindexBatchSizes...), targets
}

func TestResolveAllStartupBulkApplyUsesOneBoundedPage(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "")

	ordinaryStats, ordinaryBatches, ordinaryTargets := runStartupBulkApplyFixture(t, false)
	startupStats, startupBatches, startupTargets := runStartupBulkApplyFixture(t, true)

	if fmt.Sprint(ordinaryBatches) != "[2048 2048]" {
		t.Fatalf("ordinary reindex batches = %v, want [2048 2048]", ordinaryBatches)
	}
	if fmt.Sprint(startupBatches) != "[4096]" {
		t.Fatalf("startup reindex batches = %v, want [4096]", startupBatches)
	}
	if *ordinaryStats != *startupStats {
		t.Fatalf("stats differ: ordinary=%+v startup=%+v", ordinaryStats, startupStats)
	}
	for i := range ordinaryTargets {
		if ordinaryTargets[i] != startupTargets[i] {
			t.Fatalf("target %d differs: ordinary=%q startup=%q", i, ordinaryTargets[i], startupTargets[i])
		}
	}
}

func TestResolveAllStartupBulkApplyHonorsSmallerOperatorChunk(t *testing.T) {
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1024")

	_, batches, _ := runStartupBulkApplyFixture(t, true)
	if fmt.Sprint(batches) != "[1024 1024 1024 1024]" {
		t.Fatalf("startup reindex batches = %v, want four 1024-edge batches", batches)
	}
}
