package store_sqlite

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

func readinessStorePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "readiness.sqlite")
}

// seedReadinessStore builds a store with one repo carried through all three
// stages and returns its path.
func seedReadinessStore(t *testing.T) string {
	t.Helper()
	path := readinessStorePath(t)
	store, err := Open(path)
	require.NoError(t, err)

	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 100}))
	require.NoError(t, store.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "repoA", IndexedSHA: "abc123", IndexedAt: 1700,
	}))
	require.NoError(t, store.StampDeriveState([]graph.DeriveCompletion{{
		RepoPrefix: "repoA", DerivedSHA: "abc123", PassVersion: 3, ConfigHash: "cafe",
	}}, 1800))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types", "python-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "go-types", 1))
	require.NoError(t, store.Close())
	return path
}

func TestReadReadinessStatesReadsEveryStageInOneOpen(t *testing.T) {
	t.Parallel()
	states, err := ReadReadinessStates(seedReadinessStore(t))
	require.NoError(t, err)
	require.True(t, states.StoreFound)
	require.True(t, states.DeriveTable)
	require.True(t, states.EnrichTable)

	require.Contains(t, states.Index, "repoA")
	require.Equal(t, "abc123", states.Index["repoA"].IndexedSHA)

	repo := states.Repos["repoA"]
	require.NotZero(t, repo.Gen)
	require.Equal(t, int64(1), repo.ContentGen)
	require.True(t, repo.DeriveFound)
	require.Equal(t, int64(1), repo.Derive.DerivedContentGen)
	require.Equal(t, int64(3), repo.Derive.PassVersion)
	require.Equal(t, "cafe", repo.Derive.ConfigHash)
	require.False(t, repo.Derive.Legacy)

	// The MINIMUM, not the newest: go-types is current at 1 and python-types
	// has never run, and the reduction has to report the second.
	require.Equal(t, 2, repo.EnrichProviders)
	require.Equal(t, int64(0), repo.EnrichMinContentGen)
	require.False(t, repo.EnrichNoneDeclared)
	require.Equal(t, 2, repo.EnrichRows)
}

// Absent, empty and populated are three states, not two. A table that is not
// there means this binary is ahead of the store and knows nothing; a table that
// is there with no row for a repo means the repo has genuinely never been
// through that stage. Collapsing them would accuse every user of a missing
// derive on the upgrade that introduced the table.
func TestReadReadinessStatesTellsAnAbsentTableFromAnEmptyOne(t *testing.T) {
	t.Parallel()
	path := seedReadinessStore(t)

	states, err := ReadReadinessStates(path)
	require.NoError(t, err)
	require.True(t, states.DeriveTable)
	require.True(t, states.Repos["repoA"].DeriveFound)

	dropReadinessTable(t, path, "derive_state")
	states, err = ReadReadinessStates(path)
	require.NoError(t, err)
	require.False(t, states.DeriveTable, "absent")
	require.False(t, states.Repos["repoA"].DeriveFound)
	require.True(t, states.EnrichTable, "one absent table must not hide the others")
	require.Contains(t, states.Index, "repoA", "nor the index rows")

	dropReadinessTable(t, path, "enrichment_state")
	states, err = ReadReadinessStates(path)
	require.NoError(t, err)
	require.False(t, states.EnrichTable)
	require.Zero(t, states.Repos["repoA"].EnrichRows)
}

// A repo present in the store but never through a stage: the table is there,
// the row is not. This is "never derived", the reportable state a repo tracked
// during daemon warmup sits in permanently.
func TestReadReadinessStatesReportsARepoWithNoStageRows(t *testing.T) {
	t.Parallel()
	path := readinessStorePath(t)
	store, err := Open(path)
	require.NoError(t, err)
	require.NoError(t, store.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "repoA", IndexedSHA: "abc", IndexedAt: 1,
	}))
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 1}))
	require.NoError(t, store.Close())

	states, err := ReadReadinessStates(path)
	require.NoError(t, err)
	require.True(t, states.DeriveTable)
	require.True(t, states.EnrichTable)
	require.False(t, states.Repos["repoA"].DeriveFound, "present, but never derived")
	require.Zero(t, states.Repos["repoA"].EnrichRows)
	require.Equal(t, int64(1), states.Repos["repoA"].ContentGen)
}

// No store file is a legitimate empty answer — nothing has been indexed. A file
// that is not a database is not, and must fail the command rather than print a
// confident "never indexed" for every repo the user has.
func TestReadReadinessStatesSeparatesNoStoreFromABrokenOne(t *testing.T) {
	t.Parallel()
	states, err := ReadReadinessStates(filepath.Join(t.TempDir(), "absent.sqlite"))
	require.NoError(t, err)
	require.False(t, states.StoreFound)
	require.Empty(t, states.Index)
	require.Empty(t, states.Repos)

	broken := filepath.Join(t.TempDir(), "broken.sqlite")
	require.NoError(t, os.WriteFile(broken, []byte("this is not a database"), 0o644))
	_, err = ReadReadinessStates(broken)
	require.Error(t, err, "a corrupt store is a failure to look, not a fact about the repos")
}

// The single-table reader shares the opener, so it has to keep the exact same
// contract it had before the refactor.
func TestReadRepoIndexStatesKeepsItsContractOnTheSharedOpener(t *testing.T) {
	t.Parallel()
	states, err := ReadRepoIndexStates(filepath.Join(t.TempDir(), "absent.sqlite"))
	require.NoError(t, err)
	require.Empty(t, states)

	broken := filepath.Join(t.TempDir(), "broken.sqlite")
	require.NoError(t, os.WriteFile(broken, []byte("not a database"), 0o644))
	_, err = ReadRepoIndexStates(broken)
	require.Error(t, err)

	states, err = ReadRepoIndexStates(seedReadinessStore(t))
	require.NoError(t, err)
	require.Equal(t, "abc123", states["repoA"].IndexedSHA)
}

// dropReadinessTable simulates a store written by a daemon older than the
// feature under test. It goes at the file directly: Open would migrate the
// table straight back.
func dropReadinessTable(t *testing.T, path, table string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	defer db.Close() //nolint:errcheck // test cleanup
	_, err = db.Exec("DROP TABLE IF EXISTS " + table)
	require.NoError(t, err)
}

// A store stamped at the current schema version but missing a column this
// binary expects — the window a version gains a column after some store was
// already stamped at it. It must read like a missing table (unknown), not fail
// the whole command: a status tool that refuses to answer about ANY repo
// because one column is absent is strictly worse than one that says it cannot
// tell yet.
func TestReadReadinessStatesToleratesAColumnThisBinaryIsAheadOf(t *testing.T) {
	t.Parallel()
	path := seedReadinessStore(t)

	db, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = db.Exec(`ALTER TABLE derive_state DROP COLUMN derived_content_gen`)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	states, err := ReadReadinessStates(path)
	require.NoError(t, err, "a column this binary is ahead of must not fail the command")
	require.False(t, states.DeriveTable)
	require.True(t, states.EnrichTable, "the other stages still answer")
	require.Contains(t, states.Index, "repoA")
	require.Equal(t, int64(1), states.Repos["repoA"].ContentGen)
}
