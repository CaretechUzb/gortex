package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
)

// cloneRepoSource is a single Go file holding two near-duplicate
// functions (processItems / processRecords — identical logic, every
// identifier renamed: a textbook Type-2 clone) plus an unrelated
// function of comparable size that must NOT be flagged as a clone.
const cloneRepoSource = `package main

func processItems(items []Item) int {
	total := 0
	for i := 0; i < len(items); i++ {
		if items[i].Active {
			total += items[i].Weight * factor
		} else {
			total -= items[i].Penalty
		}
	}
	if total < 0 {
		total = 0
	}
	return total
}

func processRecords(records []Record) int {
	sum := 0
	for idx := 0; idx < len(records); idx++ {
		if records[idx].Enabled {
			sum += records[idx].Score * multiplier
		} else {
			sum -= records[idx].Fine
		}
	}
	if sum < 0 {
		sum = 0
	}
	return sum
}

func openAndScan(conn *Conn, statement string) error {
	rows, err := conn.Query(statement)
	if err != nil {
		return wrap(err, "query failed")
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if scanErr := rows.Scan(&name); scanErr != nil {
			return scanErr
		}
	}
	return rows.Err()
}
`

func similarToEdges(g graph.Store) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeSimilarTo {
			out = append(out, e)
		}
	}
	return out
}

// TestIndex_EmitsSimilarToEdges runs the full single-repo indexer over
// a file with a Type-2 clone pair and asserts the inline IndexCtx
// global pass stamped signatures and materialised symmetric
// EdgeSimilarTo edges between exactly the two clones.
func TestIndex_EmitsSimilarToEdges(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"), cloneRepoSource)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)

	itemsID := "main.go::processItems"
	recordsID := "main.go::processRecords"
	otherID := "main.go::openAndScan"

	// Signatures stamped at parse time.
	for _, id := range []string{itemsID, recordsID, otherID} {
		n := g.GetNode(id)
		require.NotNil(t, n, "node %s should exist", id)
		sig, _ := n.Meta[cloneSigMetaKey].(string)
		assert.NotEmpty(t, sig, "node %s should carry a clone_sig", id)
	}

	edges := similarToEdges(g)
	require.Len(t, edges, 2, "expected exactly one symmetric clone pair (2 directed edges)")

	// Both directions present, between the two clones only.
	dirs := map[[2]string]bool{}
	for _, e := range edges {
		dirs[[2]string{e.From, e.To}] = true
		assert.Equal(t, graph.OriginASTInferred, e.Origin)
		sim, ok := e.Meta["similarity"].(float64)
		assert.True(t, ok, "edge should carry similarity meta")
		assert.GreaterOrEqual(t, sim, clones.DefaultThreshold)
		assert.Equal(t, sim, e.Confidence)
	}
	assert.True(t, dirs[[2]string{itemsID, recordsID}], "missing processItems→processRecords")
	assert.True(t, dirs[[2]string{recordsID, itemsID}], "missing processRecords→processItems")

	// The unrelated function must not appear on any clone edge.
	for _, e := range edges {
		assert.NotEqual(t, otherID, e.From, "unrelated fn must not be a clone")
		assert.NotEqual(t, otherID, e.To, "unrelated fn must not be a clone")
	}
}

// TestIndex_CloneEdgesSurviveReindex verifies that re-indexing the file
// (eviction + re-add + global recompute) leaves the clone edge set
// consistent rather than duplicated or stale.
func TestIndex_CloneEdgesSurviveReindex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeFile(t, path, cloneRepoSource)

	g := graph.New()
	idx := newTestIndexer(g)
	_, err := idx.Index(dir)
	require.NoError(t, err)
	require.Len(t, similarToEdges(g), 2)

	// Re-index the same file: EvictFile drops the old edges in both
	// directions, the resolve pass recomputes them.
	require.NoError(t, idx.IndexFile(path))
	assert.Len(t, similarToEdges(g), 2, "reindex must not duplicate or drop clone edges")
}

// TestDetectClonesAndEmitEdges is a focused unit test over the global
// pass: it builds a graph by hand, stamps signatures, and asserts edge
// emission + idempotency without going through the parser.
func TestDetectClonesAndEmitEdges(t *testing.T) {
	g := graph.New()

	bodyA := cloneRepoSource // any substantial body works for the unit test
	sigA, ok := clones.ComputeSignature(bodyA)
	require.True(t, ok)
	enc := clones.EncodeSignature(sigA)

	// Two nodes carrying the identical signature → guaranteed clone.
	g.AddNode(&graph.Node{
		ID: "a.go::Fn", Kind: graph.KindFunction, Name: "Fn",
		FilePath: "a.go", StartLine: 1, Language: "go",
		Meta: map[string]any{cloneSigMetaKey: enc},
	})
	g.AddNode(&graph.Node{
		ID: "b.go::Gn", Kind: graph.KindMethod, Name: "Gn",
		FilePath: "b.go", StartLine: 1, Language: "go",
		Meta: map[string]any{cloneSigMetaKey: enc},
	})
	// A node with no signature must be ignored.
	g.AddNode(&graph.Node{
		ID: "c.go::Hn", Kind: graph.KindFunction, Name: "Hn",
		FilePath: "c.go", StartLine: 1, Language: "go",
	})

	stats, _ := detectClonesAndEmitEdgesWithBaselineCtx(context.Background(), g, "", 0)
	assert.Equal(t, 1, stats.Pairs)
	assert.Equal(t, 2, stats.Edges)

	// Idempotent: a second run dedupes via graph.AddEdge.
	detectClonesAndEmitEdgesWithBaselineCtx(context.Background(), g, "", 0)
	assert.Len(t, similarToEdges(g), 2, "second pass must not duplicate edges")
}

func TestBodyText(t *testing.T) {
	lines := []string{"line1", "line2", "line3", "line4"}
	assert.Equal(t, "line2\nline3", bodyText(lines, 2, 3))
	assert.Equal(t, "line1\nline2\nline3\nline4", bodyText(lines, 1, 4))
	assert.Equal(t, "", bodyText(lines, 0, 2), "zero start line is degenerate")
	assert.Equal(t, "", bodyText(lines, 3, 2), "end before start is degenerate")
	assert.Equal(t, "", bodyText(lines, 99, 100), "out-of-bounds start")
	assert.Equal(t, "line4", bodyText(lines, 4, 99), "end clamps to len")
}
