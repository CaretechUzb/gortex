package graph

import (
	"fmt"
	"testing"
)

func TestGraphScanEdgesByKindsBatchedYieldsCrossShardMoveOnce(t *testing.T) {
	g := newWithShardCount(4)
	oldFrom := sourceIDForShard(t, g, 0)
	newFrom := sourceIDForShard(t, g, g.shardCount-1)
	edge := &Edge{
		From: oldFrom, To: "target", Kind: EdgeReturnsTo,
		FilePath: "repo/file.go", Line: 17,
	}
	g.AddEdge(edge)

	visits := 0
	ScanEdgesByKindsBatched(g, []EdgeKind{EdgeReturnsTo}, 1, func(batch []*Edge) bool {
		if len(batch) != 1 || batch[0] != edge {
			t.Fatalf("batch = %#v, want the original edge", batch)
		}
		visits++
		previousFrom := edge.From
		edge.From = newFrom
		g.ReindexEdges([]EdgeReindex{{
			Edge:            edge,
			OldFrom:         previousFrom,
			OldTo:           edge.To,
			OldKind:         edge.Kind,
			OldFilePath:     edge.FilePath,
			OldLine:         edge.Line,
			RefreshIdentity: true,
		}})
		return true
	})

	if visits != 1 {
		t.Fatalf("edge visited %d times after moving from shard 0 to shard %d; want exactly once", visits, g.shardCount-1)
	}
	if got := g.GetOutEdges(oldFrom); len(got) != 0 {
		t.Fatalf("old source still has %d outgoing edges", len(got))
	}
	got := g.GetOutEdges(newFrom)
	if len(got) != 1 || got[0] != edge {
		t.Fatalf("new source outgoing edges = %#v, want the moved edge", got)
	}
}

func sourceIDForShard(t *testing.T, g *Graph, want int) string {
	t.Helper()
	for i := 0; i < 10_000; i++ {
		candidate := fmt.Sprintf("source-%d", i)
		if g.shardIdx(candidate) == want {
			return candidate
		}
	}
	t.Fatalf("could not find source id for shard %d", want)
	return ""
}
