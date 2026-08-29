package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func openGenTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "gen.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func readGen(t *testing.T, store *Store, prefix string) int64 {
	t.Helper()
	var gen int64
	err := store.db.QueryRow(`SELECT gen FROM repo_graph_gen WHERE repo_prefix = ?`, prefix).Scan(&gen)
	if err == sql.ErrNoRows {
		return 0
	}
	require.NoError(t, err)
	return gen
}

func genNode(prefix, name string) *graph.Node {
	return &graph.Node{
		ID: prefix + "/a.go::" + name, Kind: graph.KindFunction,
		FilePath: prefix + "/a.go", RepoPrefix: prefix,
	}
}

// The core invariant readiness rests on: a batch that really changes a repo's
// rows advances that repo's anchor, and a batch that changes nothing does not.
// Without the second half every repo would decay to "partial" on idle writes;
// without the first, a mutated repo would keep reading "ready".
func TestRepoGraphGenAdvancesOnlyForReposAnEffectiveBatchTouched(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	a, b := genNode("repoA", "A"), genNode("repoB", "B")
	store.AddBatch([]*graph.Node{a, b}, nil)
	genA, genB := readGen(t, store, "repoA"), readGen(t, store, "repoB")
	require.Positive(t, genA, "a batch inserting repoA nodes must advance repoA")
	require.Positive(t, genB, "a batch inserting repoB nodes must advance repoB")

	// Re-adding identical rows changes nothing, so neither anchor may move.
	store.AddBatch([]*graph.Node{a, b}, nil)
	require.Equal(t, genA, readGen(t, store, "repoA"), "a no-op batch must not advance an anchor")
	require.Equal(t, genB, readGen(t, store, "repoB"))

	// A batch naming only repoA must leave repoB exactly where it was --
	// otherwise a busy repo would drag every sibling out of "ready".
	store.AddBatch([]*graph.Node{genNode("repoA", "A2")}, nil)
	require.Greater(t, readGen(t, store, "repoA"), genA, "repoA changed and must advance")
	require.Equal(t, genB, readGen(t, store, "repoB"), "repoB was untouched and must not advance")
}

// A cross-repo edge is a change to the graph at BOTH ends: "who uses this"
// answers differently for each side, so each side's anchor must move.
func TestRepoGraphGenAdvancesBothEndsOfACrossRepoEdge(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	from, to := genNode("repoA", "A"), genNode("repoB", "B")
	store.AddBatch([]*graph.Node{from, to}, nil)
	genA, genB := readGen(t, store, "repoA"), readGen(t, store, "repoB")

	store.AddBatch(nil, []*graph.Edge{{
		From: from.ID, To: to.ID, Kind: graph.EdgeCalls, FilePath: from.FilePath, Line: 3,
	}})

	require.Greater(t, readGen(t, store, "repoA"), genA, "the calling repo's graph changed")
	require.Greater(t, readGen(t, store, "repoB"), genB, "the called repo gained an inbound edge")
}

// The bump must belong to the caller's transaction, not merely run near it.
// If it committed separately, a crash in the gap would leave a mutated graph
// sitting at the old anchor -- and every stage stamped there would read
// "ready" against a graph it no longer describes.
func TestRepoGraphGenBumpRollsBackWithItsTransaction(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	before := readGen(t, store, "repoA")
	require.Positive(t, before)

	store.writeMu.Lock()
	tx, err := store.beginWrite()
	require.NoError(t, err)
	require.NoError(t, bumpRepoGensTx(tx, []string{"repoA"}))
	require.NoError(t, tx.Rollback())
	store.writeMu.Unlock()

	require.Equal(t, before, readGen(t, store, "repoA"),
		"a rolled-back transaction must leave the anchor untouched")
}

// One transaction is one mutation: a batch naming a repo many times must
// advance it once, or the anchor would inflate with batch size rather than
// with the number of times the repo actually changed.
func TestRepoGraphGenBumpsOncePerRepoPerTransaction(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.writeMu.Lock()
	defer store.writeMu.Unlock()

	tx, err := store.beginWrite()
	require.NoError(t, err)
	require.NoError(t, bumpRepoGensTx(tx, []string{"repoA", "repoA", "repoA", ""}))
	require.NoError(t, tx.Commit())

	require.Equal(t, int64(1), readGen(t, store, "repoA"))
	require.Equal(t, int64(0), readGen(t, store, ""),
		"the empty prefix is no repository and must never get an anchor row")
}

// The store-wide fallback exists for mutations that cannot name what they
// touched. It must advance every anchor -- including a repo that has graph
// rows but no repo_index_state row yet, which happens because the index
// stamps that row only at the END of a run.
func TestBumpAllRepoGensCoversIndexedAndNotYetIndexedRepos(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.writeMu.Lock()
	defer store.writeMu.Unlock()

	_, err := store.writerDB.Exec(`INSERT INTO repo_index_state (repo_prefix) VALUES ('indexed')`)
	require.NoError(t, err)
	_, err = store.writerDB.Exec(`INSERT INTO repo_graph_gen (repo_prefix, gen) VALUES ('midindex', 4)`)
	require.NoError(t, err)

	tx, err := store.beginWrite()
	require.NoError(t, err)
	require.NoError(t, bumpAllRepoGensTx(tx))
	require.NoError(t, tx.Commit())

	require.Equal(t, int64(1), readGen(t, store, "indexed"), "seeded from repo_index_state")
	require.Equal(t, int64(5), readGen(t, store, "midindex"), "advanced despite having no index_state row")
}

// The v13 migration must be safe to re-run: applyInPlaceMigrations rolls its
// whole transaction back on any error, and the user_version stamp is a
// SEPARATE statement, so a crash between the two replays every step.
func TestCreateReadinessStateTablesIsIdempotentAndSeedsLegacyRows(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	_, err := store.writerDB.Exec(`INSERT INTO repo_index_state (repo_prefix) VALUES ('alpha'), ('beta')`)
	require.NoError(t, err)

	for run := 1; run <= 2; run++ {
		tx, err := store.writerDB.Begin()
		require.NoError(t, err)
		require.NoError(t, createReadinessStateTables(tx), "run %d", run)
		require.NoError(t, tx.Commit())

		var legacy, gens int
		require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM derive_state WHERE legacy = 1`).Scan(&legacy))
		require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM repo_graph_gen`).Scan(&gens))
		require.Equal(t, 2, legacy, "run %d: one legacy row per pre-existing repo, never duplicated", run)
		require.GreaterOrEqual(t, gens, 2, "run %d", run)
	}
}

// A repo derived before v13 existed must not claim a completion nobody
// recorded. The migration marks it legacy so readiness can say "unknown"
// rather than inventing a verdict.
func TestV13MigrationNeverClaimsAnUnrecordedDerive(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	_, err := store.writerDB.Exec(`INSERT INTO repo_index_state (repo_prefix) VALUES ('legacyrepo')`)
	require.NoError(t, err)

	tx, err := store.writerDB.Begin()
	require.NoError(t, err)
	require.NoError(t, createReadinessStateTables(tx))
	require.NoError(t, tx.Commit())

	var legacy int
	var derivedGen int64
	require.NoError(t, store.db.QueryRow(
		`SELECT legacy, derived_gen FROM derive_state WHERE repo_prefix = 'legacyrepo'`,
	).Scan(&legacy, &derivedGen))
	require.Equal(t, 1, legacy)
	require.Zero(t, derivedGen, "a legacy row must carry no derive generation to compare against")
}

// enrichment_state.gen must exist after the migration on a store that predates
// it, and the guarded ALTER must tolerate a second run.
func TestV13MigrationAddsEnrichmentGenExactlyOnce(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	for run := 1; run <= 2; run++ {
		tx, err := store.writerDB.Begin()
		require.NoError(t, err)
		require.NoError(t, createReadinessStateTables(tx), "run %d", run)
		require.NoError(t, tx.Commit())
	}
	var cols int
	require.NoError(t, store.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('enrichment_state') WHERE name = 'gen'`,
	).Scan(&cols))
	require.Equal(t, 1, cols)
}

// The anchor is only useful if the whole mutation surface maintains it, not
// just the batch-insert path. These are the families a repo actually changes
// through during ordinary operation; each must advance the anchor of the repo
// it touched, and none may advance a bystander's.
func TestRepoGraphGenIsMaintainedAcrossMutationFamilies(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		mutate  func(t *testing.T, s *Store, a, b *graph.Node, e *graph.Edge)
		wantB   bool // does repoB legitimately change too?
	}{
		{
			name: "RemoveEdgesExact",
			mutate: func(t *testing.T, s *Store, a, b *graph.Node, e *graph.Edge) {
				require.Equal(t, 1, s.RemoveEdgesExact([]*graph.Edge{e}))
			},
			wantB: true, // the edge pointed at repoB, so repoB lost an inbound edge
		},
		{
			name: "EvictFiles",
			mutate: func(t *testing.T, s *Store, a, b *graph.Node, e *graph.Edge) {
				nodes, _ := s.EvictFiles([]string{a.FilePath})
				require.Positive(t, nodes)
			},
			wantB: true, // evicting repoA's file also deletes the edge into repoB
		},
		{
			name: "RemoveEdge",
			mutate: func(t *testing.T, s *Store, a, b *graph.Node, e *graph.Edge) {
				require.True(t, s.RemoveEdge(e.From, e.To, e.Kind))
			},
			wantB: true,
		},
		{
			name: "PersistEdgeAttributes",
			mutate: func(t *testing.T, s *Store, a, b *graph.Node, e *graph.Edge) {
				updated := *e
				updated.Confidence = 0.9
				s.PersistEdgeAttributes(&updated)
			},
			wantB: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := openGenTestStore(t)
			a, b := genNode("repoA", "A"), genNode("repoB", "B")
			bystander := genNode("repoC", "C")
			edge := &graph.Edge{
				From: a.ID, To: b.ID, Kind: graph.EdgeCalls, FilePath: a.FilePath, Line: 5,
			}
			store.AddBatch([]*graph.Node{a, b, bystander}, []*graph.Edge{edge})

			genA := readGen(t, store, "repoA")
			genB := readGen(t, store, "repoB")
			genC := readGen(t, store, "repoC")
			require.Positive(t, genA)

			tc.mutate(t, store, a, b, edge)

			require.Greater(t, readGen(t, store, "repoA"), genA,
				"%s changed repoA and must advance its anchor", tc.name)
			if tc.wantB {
				require.Greater(t, readGen(t, store, "repoB"), genB,
					"%s changed repoB's inbound edges and must advance it", tc.name)
			}
			require.Equal(t, genC, readGen(t, store, "repoC"),
				"%s must not advance an untouched repo", tc.name)
		})
	}
}

// Purging a repo deletes the cross-repo edges that pointed into it, so the
// repos those edges came FROM have genuinely changed and must advance -- while
// the purged repo's own anchor row goes away with the rest of its state.
func TestPurgeRepoAdvancesSurvivorsAndDropsItsOwnAnchor(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	a, b := genNode("repoA", "A"), genNode("repoB", "B")
	store.AddBatch([]*graph.Node{a, b}, []*graph.Edge{{
		From: a.ID, To: b.ID, Kind: graph.EdgeCalls, FilePath: a.FilePath, Line: 5,
	}})
	genA := readGen(t, store, "repoA")
	require.Positive(t, readGen(t, store, "repoB"))

	require.NoError(t, store.PurgeRepo("repoB"))

	require.Greater(t, readGen(t, store, "repoA"), genA,
		"repoA lost an outbound edge and must advance")
	var rows int
	require.NoError(t, store.db.QueryRow(
		`SELECT COUNT(*) FROM repo_graph_gen WHERE repo_prefix = 'repoB'`).Scan(&rows))
	require.Zero(t, rows, "a purged repo must leave no anchor row behind")
}

// toRepoExpr restates graph.RepoPrefixOfID in SQL because edges has no
// generated to_repo column. Restating it means it can drift, so pin the two
// together the way TestEdgeScopeColumnsMirrorGoHelpers pins from_repo.
func TestToRepoExprMirrorsRepoPrefixOfID(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	for _, id := range []string{
		"repoA/a.go::A",
		"repoB/pkg/deep/b.go::B",
		"unresolved::Target",
		"dep::github.com/x/y",
		"external::thing",
		"/leadingslash",
		"noslash",
		"",
	} {
		var got string
		require.NoError(t, store.db.QueryRow(
			`SELECT `+strings.Replace(toRepoExpr, "to_id", "?", -1), id, id, id, id).Scan(&got))
		require.Equal(t, graph.RepoPrefixOfID(id), got,
			"SQL and Go must agree on the repo prefix of %q", id)
	}
}
