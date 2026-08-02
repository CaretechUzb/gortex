package graph

import (
	"math"
	"testing"
)

func TestRepoMemoryEstimateTotalSaturates(t *testing.T) {
	if got := (RepoMemoryEstimate{NodeBytes: 11, EdgeBytes: 7}).Total(); got != 18 {
		t.Fatalf("ordinary total = %d, want 18", got)
	}
	if got := (RepoMemoryEstimate{NodeBytes: math.MaxUint64 - 1, EdgeBytes: 2}).Total(); got != math.MaxUint64 {
		t.Fatalf("overflowing total = %d, want %d", got, uint64(math.MaxUint64))
	}
}

func TestRepoMemoryEstimateCrossShardAggregationSaturates(t *testing.T) {
	g := newWithShardCount(2)
	const prefix = "overflow"
	g.shards[0].repoNodeBytes[prefix] = math.MaxUint64 - 3
	g.shards[1].repoNodeBytes[prefix] = 10
	g.shards[0].repoEdgeBytes[prefix] = math.MaxUint64 - 7
	g.shards[1].repoEdgeBytes[prefix] = 16
	g.shards[0].repoNodeCount[prefix] = 1
	g.shards[1].repoNodeCount[prefix] = 2
	g.shards[0].repoEdgeCount[prefix] = 3
	g.shards[1].repoEdgeCount[prefix] = 4

	one := g.RepoMemoryEstimate(prefix)
	if one.NodeBytes != math.MaxUint64 || one.EdgeBytes != math.MaxUint64 {
		t.Fatalf("single estimate did not saturate: %+v", one)
	}
	if one.NodeCount != 3 || one.EdgeCount != 7 {
		t.Fatalf("single estimate counts changed: %+v", one)
	}
	if one.Total() != math.MaxUint64 {
		t.Fatalf("single estimate total = %d, want %d", one.Total(), uint64(math.MaxUint64))
	}

	all := g.AllRepoMemoryEstimates()[prefix]
	if all.NodeBytes != math.MaxUint64 || all.EdgeBytes != math.MaxUint64 {
		t.Fatalf("all-repo estimate did not saturate: %+v", all)
	}
	if all.NodeCount != 3 || all.EdgeCount != 7 {
		t.Fatalf("all-repo estimate counts changed: %+v", all)
	}
}
