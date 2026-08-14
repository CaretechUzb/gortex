package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// Clone detection is PER-REPOSITORY: a near-duplicate body that appears
// once in repoA and once in repoB must NOT be linked by an EdgeSimilarTo
// edge, even though the two bodies are textbook Type-2 clones of each
// other. Within each repo, genuine clone pairs are still detected.
//
// These fixtures build two repos that share one graph (prefixes "repoA"
// and "repoB"). Each repo holds:
//
//   - a within-repo Type-2 clone pair (every identifier renamed, control
//     flow identical) that MUST emit EdgeSimilarTo, and
//   - a "crossDup" body that is near-identical across the two repos — the
//     cross-repo near-dup that per-repo scoping must keep unlinked.

// repoA within-repo Type-2 clone pair: sumActiveItems / sumEnabledRecords.
const mrRepoAClone1 = `package main

func sumActiveItems(items []Item) int {
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
`

const mrRepoAClone2 = `package main

func sumEnabledRecords(records []Record) int {
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
`

// repoB within-repo Type-2 clone pair: scanOpenRows / scanLiveRows. A
// distinct shape from repoA's pair so each repo's within-repo clone is
// independent of the other's.
const mrRepoBClone1 = `package main

func scanOpenRows(conn *Conn, statement string) error {
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

const mrRepoBClone2 = `package main

func scanLiveRows(handle *Handle, query string) error {
	cursor, qerr := handle.Run(query)
	if qerr != nil {
		return wrap(qerr, "run failed")
	}
	defer cursor.Close()
	for cursor.Next() {
		var label string
		if readErr := cursor.Read(&label); readErr != nil {
			return readErr
		}
	}
	return cursor.Err()
}
`

// crossDup is one body that is parsed into BOTH repos. The repoA copy and
// the repoB copy are near-identical (Type-2 clone of each other) — the
// cross-repo near-dup whose link must be suppressed by per-repo scoping.
// To make it a real Type-2 clone across repos (not byte-identical, which
// would also collide intra-repo), repoA uses one identifier set and repoB
// another.
const mrCrossDupA = `package main

func computeDelta(values []float64, base float64) float64 {
	acc := 0.0
	for k := 0; k < len(values); k++ {
		if values[k] > base {
			acc += values[k] - base
		} else {
			acc -= base - values[k]
		}
	}
	if acc < 0 {
		acc = 0
	}
	return acc
}
`

const mrCrossDupB = `package main

func computeSpread(samples []float64, pivot float64) float64 {
	agg := 0.0
	for m := 0; m < len(samples); m++ {
		if samples[m] > pivot {
			agg += samples[m] - pivot
		} else {
			agg -= pivot - samples[m]
		}
	}
	if agg < 0 {
		agg = 0
	}
	return agg
}
`

// writeMultiRepoCloneFixture lays out two repo directories under root and
// returns their absolute file paths in stable per-repo order.
func writeMultiRepoCloneFixture(t *testing.T, root string) (repoADir string, repoAFiles []string, repoBDir string, repoBFiles []string) {
	t.Helper()
	repoADir = filepath.Join(root, "repoA")
	repoBDir = filepath.Join(root, "repoB")
	require.NoError(t, os.MkdirAll(repoADir, 0o755))
	require.NoError(t, os.MkdirAll(repoBDir, 0o755))

	wa := func(name, body string) {
		p := filepath.Join(repoADir, name)
		writeFile(t, p, body)
		repoAFiles = append(repoAFiles, p)
	}
	wb := func(name, body string) {
		p := filepath.Join(repoBDir, name)
		writeFile(t, p, body)
		repoBFiles = append(repoBFiles, p)
	}

	wa("clone1.go", mrRepoAClone1)
	wa("clone2.go", mrRepoAClone2)
	wa("crossdup.go", mrCrossDupA)

	wb("clone1.go", mrRepoBClone1)
	wb("clone2.go", mrRepoBClone2)
	wb("crossdup.go", mrCrossDupB)
	return repoADir, repoAFiles, repoBDir, repoBFiles
}

// edgeCrossesRepos reports whether a directed edge connects a repoA node
// to a repoB node (in either direction), keyed off the node RepoPrefix.
func edgeCrossesRepos(g graph.Store, e *graph.Edge) bool {
	from := g.GetNode(e.From)
	to := g.GetNode(e.To)
	if from == nil || to == nil {
		return false
	}
	return from.RepoPrefix != to.RepoPrefix
}

// assertNoCrossRepoSimilarEdge fails if any EdgeSimilarTo edge connects a
// node in one repo to a node in another.
func assertNoCrossRepoSimilarEdge(t *testing.T, g graph.Store) {
	t.Helper()
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeSimilarTo {
			continue
		}
		if edgeCrossesRepos(g, e) {
			from := g.GetNode(e.From)
			to := g.GetNode(e.To)
			t.Fatalf("cross-repo EdgeSimilarTo leaked: %s (%s) -> %s (%s)",
				e.From, from.RepoPrefix, e.To, to.RepoPrefix)
		}
	}
}

// repoSimilarEdgeSet returns the EdgeSimilarTo directed-edge set whose
// endpoints both live in repoPrefix.
func repoSimilarEdgeSet(g graph.Store, repoPrefix string) map[[2]string]struct{} {
	set := make(map[[2]string]struct{})
	for _, e := range g.AllEdges() {
		if e.Kind != graph.EdgeSimilarTo {
			continue
		}
		from := g.GetNode(e.From)
		to := g.GetNode(e.To)
		if from == nil || to == nil {
			continue
		}
		if from.RepoPrefix != repoPrefix || to.RepoPrefix != repoPrefix {
			continue
		}
		set[[2]string{e.From, e.To}] = struct{}{}
	}
	return set
}

// newRepoIndexer builds a test indexer bound to a repo prefix and sharing
// the given graph — the multi-repo setup MultiIndexer drives in production.
func newRepoIndexer(g graph.Store, prefix string) *Indexer {
	idx := newTestIndexer(g)
	idx.SetRepoPrefix(prefix)
	return idx
}

// TestClones_PerRepo_NoCrossRepoEdges is the per-repository clone-scoping
// test. Two repos share one graph; each has a within-repo Type-2 clone
// pair plus a cross-repo near-duplicate function. Running the per-repo
// batch pass (mirroring MultiIndexer.RunGlobalGraphPasses' loop) must:
//
//	(a) emit the within-repo clone pair as EdgeSimilarTo in EACH repo;
//	(b) emit NO EdgeSimilarTo edge between a repoA node and a repoB node;
//	(c) produce, via the per-repo incremental path (Rebuild then a file
//	    reindex), the SAME EdgeSimilarTo set the per-repo batch produced.
func TestClones_PerRepo_NoCrossRepoEdges(t *testing.T) {
	ctx := context.Background()

	// ---- (1) Batch path: two indexers share graph gBatch. -------------
	// SetDeferGlobalPasses(true) so Index() only parses + stamps shingles;
	// the clone pass is then driven manually per repo, exactly as
	// MultiIndexer.RunGlobalGraphPasses does.
	root := t.TempDir()
	repoADir, _, repoBDir, _ := writeMultiRepoCloneFixture(t, root)

	gBatch := graph.New()
	idxA := newRepoIndexer(gBatch, "repoA")
	idxA.SetDeferGlobalPasses(true)
	idxB := newRepoIndexer(gBatch, "repoB")
	idxB.SetDeferGlobalPasses(true)
	_, err := idxA.Index(repoADir)
	require.NoError(t, err)
	_, err = idxB.Index(repoBDir)
	require.NoError(t, err)

	// Per-repo batch clone pass (the new MultiIndexer loop).
	csA, _ := detectClonesAndEmitEdgesWithBaselineCtx(ctx, gBatch, "repoA", 0)
	csB, _ := detectClonesAndEmitEdgesWithBaselineCtx(ctx, gBatch, "repoB", 0)
	require.Positive(t, csA.Items, "repoA must have clone-eligible bodies")
	require.Positive(t, csB.Items, "repoB must have clone-eligible bodies")

	batchA := repoSimilarEdgeSet(gBatch, "repoA")
	batchB := repoSimilarEdgeSet(gBatch, "repoB")

	// (a) Within-repo clone pairs emitted in each repo (non-vacuity).
	require.GreaterOrEqual(t, len(batchA), 1,
		"repoA must emit >=1 within-repo EdgeSimilarTo")
	require.GreaterOrEqual(t, len(batchB), 1,
		"repoB must emit >=1 within-repo EdgeSimilarTo")
	// The within-repo pair is symmetric, so we expect exactly the two
	// directed edges of repoA's sumActiveItems<->sumEnabledRecords pair.
	assert.Contains(t, batchA, [2]string{"repoA/clone1.go::sumActiveItems", "repoA/clone2.go::sumEnabledRecords"})
	assert.Contains(t, batchA, [2]string{"repoA/clone2.go::sumEnabledRecords", "repoA/clone1.go::sumActiveItems"})
	assert.Contains(t, batchB, [2]string{"repoB/clone1.go::scanOpenRows", "repoB/clone2.go::scanLiveRows"})
	assert.Contains(t, batchB, [2]string{"repoB/clone2.go::scanLiveRows", "repoB/clone1.go::scanOpenRows"})

	// (b) No EdgeSimilarTo edge crosses the repo boundary. The crossDup
	// bodies are Type-2 clones of each other but live in different repos,
	// so per-repo scoping must never form that candidate pair.
	assertNoCrossRepoSimilarEdge(t, gBatch)

	// ---- (2) Incremental path: a fresh graph, per-repo Rebuild + reindex.
	// deferGlobalPasses=false so the cold Index() runs each repo's inline
	// per-repo clone pass and seeds its incremental index (Rebuild); a
	// subsequent IndexFile then drives EvictFuncs/UpdateFuncs.
	root2 := t.TempDir()
	repoADir2, repoAFiles2, repoBDir2, repoBFiles2 := writeMultiRepoCloneFixture(t, root2)

	gInc := graph.New()
	incA := newRepoIndexer(gInc, "repoA")
	incB := newRepoIndexer(gInc, "repoB")
	_, err = incA.Index(repoADir2)
	require.NoError(t, err)
	_, err = incB.Index(repoBDir2)
	require.NoError(t, err)
	require.True(t, incA.cloneIndex.built, "repoA incremental index must be built")
	require.True(t, incB.cloneIndex.built, "repoB incremental index must be built")

	// Drive each repo's files through the incremental maintainer.
	for _, f := range repoAFiles2 {
		require.NoError(t, incA.IndexFile(f))
	}
	for _, f := range repoBFiles2 {
		require.NoError(t, incB.IndexFile(f))
	}

	// (c) The per-repo incremental edge set equals the per-repo batch set,
	// and still no cross-repo edge appears.
	incEdgesA := repoSimilarEdgeSet(gInc, "repoA")
	incEdgesB := repoSimilarEdgeSet(gInc, "repoB")
	assert.Equal(t, batchA, incEdgesA,
		"repoA incremental EdgeSimilarTo set must equal the batch set")
	assert.Equal(t, batchB, incEdgesB,
		"repoB incremental EdgeSimilarTo set must equal the batch set")
	assertNoCrossRepoSimilarEdge(t, gInc)

	// Guard the directory names are wired through (the fixture writer
	// returns absolute repo dirs used above) so a refactor that drops a
	// repo can't silently make this test vacuous.
	require.NotEqual(t, repoADir2, repoBDir2)
}
