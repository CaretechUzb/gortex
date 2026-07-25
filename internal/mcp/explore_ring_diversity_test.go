package mcp

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func ringNode(id, file string) *graph.Node {
	return &graph.Node{ID: id, Name: id, Kind: graph.KindFunction, FilePath: file}
}

// One file must not describe the whole neighbourhood: the symbol a request is
// about is often in the second file, inside the full ring but past the prefix
// a saturated first file would leave.
func TestRingProjectionLetsASecondFileIntoASaturatedRing(t *testing.T) {
	nodes := []*graph.Node{
		ringNode("a1", "storage/disk.go"),
		ringNode("a2", "storage/disk.go"),
		ringNode("a3", "storage/disk.go"),
		ringNode("a4", "storage/disk.go"),
		ringNode("b1", "storage/cache.go"),
	}
	out, complete := ringNeighborsProjection(nodes, "self", 5)

	require.True(t, complete, "every eligible neighbour fits, so the ring is complete")
	require.Len(t, out, 5, "diversity reorders the ring, it must not shrink it")

	var files []string
	for _, n := range out {
		files = append(files, n.FilePath)
	}
	require.Contains(t, files, "storage/cache.go",
		"the crowded-out file must reach the exposed ring")
	require.Equal(t, "storage/cache.go", out[ringFileShare].FilePath,
		"the second file takes its turn as soon as the first has had its share")
}

// Holding a neighbour back is not dropping it. Once the spread is covered the
// crowded file's remaining members fill whatever slots are left.
func TestRingProjectionReturnsCrowdedNeighboursToLeftoverSlots(t *testing.T) {
	nodes := []*graph.Node{
		ringNode("a1", "storage/disk.go"),
		ringNode("a2", "storage/disk.go"),
		ringNode("a3", "storage/disk.go"),
		ringNode("b1", "storage/cache.go"),
	}
	out, complete := ringNeighborsProjection(nodes, "self", 4)

	require.True(t, complete)
	require.Len(t, out, 4)
	seen := map[string]bool{}
	for _, n := range out {
		seen[n.ID] = true
	}
	for _, id := range []string{"a1", "a2", "a3", "b1"} {
		require.True(t, seen[id], "neighbour %s was dropped rather than deferred", id)
	}
}

// A ring that genuinely overflows still reports itself incomplete, so callers
// cannot claim uniqueness from a saturated projection.
func TestRingProjectionStillReportsASaturatedRing(t *testing.T) {
	nodes := make([]*graph.Node, 0, 12)
	for index := 0; index < 12; index++ {
		nodes = append(nodes, ringNode(fmt.Sprintf("n%d", index), fmt.Sprintf("pkg/file%d.go", index)))
	}
	out, complete := ringNeighborsProjection(nodes, "self", 5)

	require.Len(t, out, 5)
	require.False(t, complete, "an overflowing ring must not claim completeness")
}

// Diversity changes which neighbours are shown, not which ones are eligible:
// the focus symbol itself stays out of its own ring.
func TestRingProjectionKeepsRejectingSelf(t *testing.T) {
	nodes := []*graph.Node{
		ringNode("self", "storage/disk.go"),
		ringNode("a1", "storage/disk.go"),
		ringNode("b1", "storage/cache.go"),
	}
	out, _ := ringNeighborsProjection(nodes, "self", 5)
	require.Len(t, out, 2)
	for _, n := range out {
		require.NotEqual(t, "self", n.ID, "a symbol must not appear on its own ring")
	}
}
