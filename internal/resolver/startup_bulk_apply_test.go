package resolver

import (
	"context"
	"fmt"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

const resolveApplyFixtureEdges = 4096

type resolveApplyPagerStore struct {
	*graph.Graph
	pending           []*graph.Edge
	reindexBatchSizes []int
}

func (s *resolveApplyPagerStore) BeginUnresolvedEdgeScan(context.Context) (graph.UnresolvedEdgeScan, error) {
	return graph.UnresolvedEdgeScan{
		HighWaterID:   int64(len(s.pending)),
		PendingBefore: len(s.pending),
	}, nil
}

func (s *resolveApplyPagerStore) ReadUnresolvedEdgePage(
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

func (s *resolveApplyPagerStore) ReindexEdges(batch []graph.EdgeReindex) {
	s.reindexBatchSizes = append(s.reindexBatchSizes, len(batch))
	s.Graph.ReindexEdges(batch)
}

func runResolveApplyFixture(t *testing.T) (*ResolveStats, []int) {
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

	pending := make([]*graph.Edge, 0, resolveApplyFixtureEdges)
	for i := 0; i < resolveApplyFixtureEdges; i++ {
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

	store := &resolveApplyPagerStore{Graph: base, pending: pending}
	stats := New(store).ResolveAll()

	for i, edge := range pending {
		if edge.To != target.ID {
			t.Fatalf("edge %d target = %q, want %q", i, edge.To, target.ID)
		}
	}
	return stats, append([]int(nil), store.reindexBatchSizes...)
}

func TestResolveAllUsesBoundedApplySlices(t *testing.T) {
	t.Setenv("GORTEX_BACKEND_RESOLVER", "0")
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "")

	stats, batches := runResolveApplyFixture(t)
	if stats.Resolved != resolveApplyFixtureEdges {
		t.Fatalf("resolved = %d, want %d", stats.Resolved, resolveApplyFixtureEdges)
	}
	if fmt.Sprint(batches) != "[2048 2048]" {
		t.Fatalf("reindex batches = %v, want [2048 2048]", batches)
	}
}

func TestResolveAllHonorsSmallerOperatorChunk(t *testing.T) {
	t.Setenv("GORTEX_BACKEND_RESOLVER", "0")
	t.Setenv("GORTEX_RESOLVE_CHUNK", "1")
	t.Setenv("GORTEX_RESOLVE_CHUNK_SIZE", "1024")

	stats, batches := runResolveApplyFixture(t)
	if stats.Resolved != resolveApplyFixtureEdges {
		t.Fatalf("resolved = %d, want %d", stats.Resolved, resolveApplyFixtureEdges)
	}
	if fmt.Sprint(batches) != "[1024 1024 1024 1024]" {
		t.Fatalf("reindex batches = %v, want four 1024-edge batches", batches)
	}
}
