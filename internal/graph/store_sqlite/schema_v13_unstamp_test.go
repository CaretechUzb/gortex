package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// The unstamp repair, against the rows a live store actually held.
//
// It shipped as v14 on this branch and is now folded into the merged v13,
// alongside main's coverage purge and the readiness setup — so these tests
// rewind to 12 to make Open reconcile through it.
//
// `external-call::module::go:odoo` was stamped repo_prefix `external-call` —
// graph.StubRepoPrefix read the synthetic namespace as a repository, and the
// Go external-call attribution pass carried that value onto the node it
// materialised. Owning a node is how a prefix earns a repo_graph_gen row, so
// `external-call` got one, and bumpAllRepoGensTx then advanced it on every
// store-wide mutation for the life of the store.
//
// Reopening must clear both, and must leave every real repository alone.
func TestReopeningUnstampsASyntheticNamespaceClaimedAsARepo(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "unstamp.sqlite")

	store, err := Open(path)
	require.NoError(t, err)
	seedPoisonedOwnership(t, store)
	// Rewind past the repair so reopening has to run it. 12 rather than 13:
	// the repair lives INSIDE v13 now, and a store already stamped 13 is
	// stored == current, which reconciles nothing.
	rewindSchemaVersion(t, store, 12)
	require.NoError(t, store.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	defer reopened.Close() //nolint:errcheck

	require.Equal(t, "", nodeRepoPrefix(t, reopened, "external-call::module::go:odoo"),
		"a node inside a synthetic namespace is owned by no repository")
	require.Equal(t, "local", nodeRepoPrefix(t, reopened, "local::module::go:re"),
		"a real repo's synthetic node keeps the owner the v5 backfill gave it correctly")

	require.False(t, anchorRowExists(t, reopened, "external-call"),
		"the anchor row was earned by owning a node; with the claim gone it must go too")
	require.True(t, anchorRowExists(t, reopened, "local"),
		"a real repository's anchor is untouched — dropping one would make its "+
			"next readiness verdict read never-derived")
}

// Every reserved namespace is repaired, not just the one that was observed.
// The migration reads its list from graph.ReservedIDNamespaces, so this is what
// catches a segment added to the parser and forgotten by the repair.
func TestTheRepairCoversEveryReservedNamespace(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "all.sqlite")

	store, err := Open(path)
	require.NoError(t, err)
	for _, ns := range graph.ReservedIDNamespaces() {
		insertNode(t, store, ns+"::module::go:x", ns)
		insertAnchor(t, store, ns)
	}
	rewindSchemaVersion(t, store, 12)
	require.NoError(t, store.Close())

	reopened, err := Open(path)
	require.NoError(t, err)
	defer reopened.Close() //nolint:errcheck

	for _, ns := range graph.ReservedIDNamespaces() {
		require.Equal(t, "", nodeRepoPrefix(t, reopened, ns+"::module::go:x"), ns)
		require.False(t, anchorRowExists(t, reopened, ns), ns)
	}
}

// A second pass matches nothing and changes nothing. Every in-place step must
// be idempotent — the registry says so — and this one is re-runnable by
// construction only because the first pass removes what the predicate selects.
func TestTheRepairIsIdempotent(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "twice.sqlite"))
	require.NoError(t, err)
	defer store.Close() //nolint:errcheck
	seedPoisonedOwnership(t, store)

	for range 2 {
		store.writeMu.Lock()
		tx, err := store.beginWrite()
		require.NoError(t, err)
		require.NoError(t, unstampReservedRepoPrefixes(tx))
		require.NoError(t, tx.Commit())
		store.writeMu.Unlock()

		require.Equal(t, "", nodeRepoPrefix(t, store, "external-call::module::go:odoo"))
		require.Equal(t, "local", nodeRepoPrefix(t, store, "local::module::go:re"))
		require.False(t, anchorRowExists(t, store, "external-call"))
		require.True(t, anchorRowExists(t, store, "local"))
	}
}

// seedPoisonedOwnership writes one node of each kind the repair must tell
// apart, plus the anchor rows they earned.
func seedPoisonedOwnership(t *testing.T, s *Store) {
	t.Helper()
	insertNode(t, s, "external-call::module::go:odoo", "external-call")
	insertNode(t, s, "local::module::go:re", "local")
	insertAnchor(t, s, "external-call")
	insertAnchor(t, s, "local")
}

// The poison is written straight to the tables, not through AddBatch: the
// write path no longer produces this shape, which is the point of the fix. A
// store that already carries it is what the repair exists for.
func insertNode(t *testing.T, s *Store, id, repoPrefix string) {
	t.Helper()
	writeTx(t, s,
		`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES (?, 'module', 'x', '', ?)`,
		id, repoPrefix)
}

func insertAnchor(t *testing.T, s *Store, prefix string) {
	t.Helper()
	writeTx(t, s, `INSERT INTO repo_graph_gen (repo_prefix, gen, content_gen) VALUES (?, 5, 0)`,
		prefix)
}

// rewindSchemaVersion stamps an older version so the next Open has to
// reconcile forward. The pragma goes to the writer, like every other write.
func rewindSchemaVersion(t *testing.T, s *Store, v int) {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	require.NoError(t, setUserVersion(s.writerDB, v))
}

// writeTx runs one statement on the writer connection. s.db is the read pool
// and rejects writes outright.
func writeTx(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	require.NoError(t, err)
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	_, err = tx.Exec(query, args...)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func nodeRepoPrefix(t *testing.T, s *Store, id string) string {
	t.Helper()
	var prefix string
	require.NoError(t, s.db.QueryRow(`SELECT repo_prefix FROM nodes WHERE id = ?`, id).Scan(&prefix))
	return prefix
}

func anchorRowExists(t *testing.T, s *Store, prefix string) bool {
	t.Helper()
	var n int
	require.NoError(t, s.db.QueryRow(
		`SELECT COUNT(*) FROM repo_graph_gen WHERE repo_prefix = ?`, prefix).Scan(&n))
	return n > 0
}
