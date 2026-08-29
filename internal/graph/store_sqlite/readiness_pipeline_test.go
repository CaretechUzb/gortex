package store_sqlite

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// readContentGen returns a repo's content counter — the value every stage stamp
// is compared against.
func readContentGen(t *testing.T, store *Store, prefix string) int64 {
	t.Helper()
	var gen int64
	err := store.db.QueryRow(
		`SELECT content_gen FROM repo_graph_gen WHERE repo_prefix = ?`, prefix).Scan(&gen)
	if err == sql.ErrNoRows {
		return 0
	}
	require.NoError(t, err)
	return gen
}

// indexFiles plays the indexer's content write: nodes land, then the mtimes
// that say those files were parsed. Both halves matter — the mtime write is
// what advances content_gen, and it is deliberately a separate transaction from
// the node write, exactly as the real pipeline has it.
func indexFiles(t *testing.T, store *Store, prefix string, mtimes map[string]int64) {
	t.Helper()
	nodes := make([]*graph.Node, 0, len(mtimes))
	for path := range mtimes {
		nodes = append(nodes, &graph.Node{
			ID: prefix + "/" + path + "::F", Kind: graph.KindFunction,
			FilePath: prefix + "/" + path, RepoPrefix: prefix,
		})
	}
	store.AddBatch(nodes, nil)
	require.NoError(t, store.BulkSetFileMtimes(prefix, mtimes))
}

// derivedEdgesLand plays a derived pass emitting edges, which is a graph write
// like any other: it advances gen and must NOT advance content_gen.
func derivedEdgesLand(t *testing.T, store *Store, prefix string, n int) {
	t.Helper()
	edges := make([]*graph.Edge, 0, n)
	for i := range n {
		edges = append(edges, &graph.Edge{
			From: prefix + "/a.go::F", To: prefix + "/b.go::F",
			Kind: graph.EdgeImplements, Line: i + 1,
		})
	}
	store.AddBatch(nil, edges)
}

// THE oracle. Every earlier test in this feature exercised one stage against a
// fake; this one runs index -> derive -> enrich in sequence against one real
// store and asserts the repo ends up READY.
//
// Two designs died here. Comparing a stage stamp against repo_graph_gen.gen
// fails at the third step: enrichment's own edges push gen past the derive's
// stamp, so a perfectly healthy repo reads "partial" forever and nothing
// repairs it — a re-derive over unchanged content inserts nothing, so it moves
// nothing. Stamping gen at derive START instead fails at the second step, for
// the same reason one line earlier. Only a counter that no stage's own output
// can move survives the whole sequence.
func TestPipelineLeavesEveryStageCurrent(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	// 1. index
	indexFiles(t, store, "repoA", map[string]int64{"a.go": 100, "b.go": 200})
	contentAtIndex := readContentGen(t, store, "repoA")
	require.NotZero(t, contentAtIndex, "indexing files must advance the content counter")

	// 2. derive — its own edges land, then it stamps.
	derivedEdgesLand(t, store, "repoA", 3)
	require.NoError(t, store.StampDeriveState(
		[]graph.DeriveCompletion{{RepoPrefix: "repoA", DerivedSHA: "sha1", PassVersion: 1}}, 1700))

	derive, found, err := store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.True(t, found)
	require.GreaterOrEqual(t, derive.DerivedContentGen, contentAtIndex,
		"a derive that just ran must cover the content it read")

	// 3. enrich — its edges land too. This is the step that broke the first
	// two designs.
	derivedEdgesLand(t, store, "repoA", 5)

	gen, contentGen, found, err := store.GetRepoGraphGen("repoA")
	require.NoError(t, err)
	require.True(t, found)
	require.Greater(t, gen, derive.DerivedGen,
		"the graph counter has moved on — that is expected and must not matter")
	require.Equal(t, contentAtIndex, contentGen,
		"no stage's own output may advance the content counter")
	require.GreaterOrEqual(t, derive.DerivedContentGen, contentGen,
		"READY: the derive still covers the current content")

	// 4. a file is saved. NOW the derive must read stale — this is the case the
	// whole feature exists for, and it must survive the fix that made step 3
	// pass.
	indexFiles(t, store, "repoA", map[string]int64{"a.go": 999, "b.go": 200})
	require.Greater(t, readContentGen(t, store, "repoA"), derive.DerivedContentGen,
		"PARTIAL: an ordinary edit must strand the derive's stamp")

	// 5. a re-derive closes it again.
	require.NoError(t, store.StampDeriveState(
		[]graph.DeriveCompletion{{RepoPrefix: "repoA", DerivedSHA: "sha2", PassVersion: 1}}, 1800))
	derive, _, err = store.GetDeriveState("repoA")
	require.NoError(t, err)
	require.Equal(t, readContentGen(t, store, "repoA"), derive.DerivedContentGen)
}

// A warm restart re-persists the authoritative mtime snapshot on every start.
// If that advanced the content counter, every stage would read stale after
// every daemon restart — the column would cry wolf on the single most common
// event in the daemon's life, and users would learn to ignore it.
func TestAnIdenticalRepersistDoesNotMoveTheContentCounter(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	snapshot := map[string]int64{"a.go": 100, "b.go": 200}
	require.NoError(t, store.ReplaceFileMtimes("repoA", snapshot))
	first := readContentGen(t, store, "repoA")
	require.NotZero(t, first)

	require.NoError(t, store.ReplaceFileMtimes("repoA", snapshot))
	require.Equal(t, first, readContentGen(t, store, "repoA"),
		"re-persisting an identical snapshot is not a content change")

	require.NoError(t, store.BulkSetFileMtimes("repoA", snapshot))
	require.Equal(t, first, readContentGen(t, store, "repoA"),
		"an upsert of unchanged mtimes is not a content change either")

	// The three ways content genuinely moves.
	require.NoError(t, store.ReplaceFileMtimes("repoA", map[string]int64{"a.go": 101, "b.go": 200}))
	changed := readContentGen(t, store, "repoA")
	require.Greater(t, changed, first, "a modified file is a content change")

	require.NoError(t, store.ReplaceFileMtimes("repoA", map[string]int64{"a.go": 101}))
	pruned := readContentGen(t, store, "repoA")
	require.Greater(t, pruned, changed, "a pruned file is a content change")

	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"c.go": 300}))
	added := readContentGen(t, store, "repoA")
	require.Greater(t, added, pruned, "a new file is a content change")

	require.NoError(t, store.DeleteFileMtimes("repoA", []string{"c.go"}))
	require.Greater(t, readContentGen(t, store, "repoA"), added, "a deleted file is a content change")

	require.NoError(t, store.DeleteFileMtimes("repoA", []string{"never-existed.go"}))
	require.Equal(t, added+1, readContentGen(t, store, "repoA"),
		"deleting nothing is not a content change")
}

// The separation, stated directly: graph writes move gen and leave content_gen
// alone; file bookkeeping moves both. Nothing else in the store may write a
// file mtime, which is what makes the second half hold without every future
// pass author having to know about it.
func TestGraphWritesMoveGenButNeverContentGen(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.NotZero(t, readGen(t, store, "repoA"))
	require.Zero(t, readContentGen(t, store, "repoA"),
		"a node batch is a graph mutation, not a record that a file was parsed")

	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 1}))
	require.Equal(t, int64(1), readContentGen(t, store, "repoA"))

	genBefore := readGen(t, store, "repoA")
	store.AddBatch([]*graph.Node{genNode("repoA", "B")}, nil)
	require.Greater(t, readGen(t, store, "repoA"), genBefore)
	require.Equal(t, int64(1), readContentGen(t, store, "repoA"))
}

// SetFileMtime is the single-row convenience form. It must go through the same
// bookkeeping as the bulk form — a second statement-only path here is exactly
// how a counter like this rots.
func TestSetFileMtimeAdvancesTheContentCounter(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	require.NoError(t, store.SetFileMtime("repoA", "a.go", 100))
	require.Equal(t, int64(1), readContentGen(t, store, "repoA"))

	require.NoError(t, store.SetFileMtime("repoA", "a.go", 100))
	require.Equal(t, int64(1), readContentGen(t, store, "repoA"), "an unchanged rewrite is not a change")

	require.NoError(t, store.SetFileMtime("repoA", "a.go", 200))
	require.Equal(t, int64(2), readContentGen(t, store, "repoA"))

	mtimes, err := store.FileMtimes("repoA")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"a.go": 200}, mtimes)
}

// One repo's content moving must not move another's. The mtime writers are all
// single-repo by signature, so this holds structurally — pinned because the gen
// counter next door deliberately does the opposite (a cross-repo edge advances
// both endpoints), and the two are easy to conflate.
func TestContentGenIsPerRepo(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 1}))
	require.NoError(t, store.BulkSetFileMtimes("repoB", map[string]int64{"b.go": 1}))
	require.Equal(t, int64(1), readContentGen(t, store, "repoA"))
	require.Equal(t, int64(1), readContentGen(t, store, "repoB"))

	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 2}))
	require.Equal(t, int64(2), readContentGen(t, store, "repoA"))
	require.Equal(t, int64(1), readContentGen(t, store, "repoB"))
}
