package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFreshStoreOpensAtCurrentSchemaVersionWithReadinessTables(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "e2e.sqlite")
	store, err := Open(path)
	require.NoError(t, err)
	defer store.Close() //nolint:errcheck

	var v int
	require.NoError(t, store.db.QueryRow(`PRAGMA user_version`).Scan(&v))
	require.Equal(t, currentSchemaVersion, v)
	require.GreaterOrEqual(t, v, 13,
		"the readiness tables arrived in v13; a store below it is not reconciled")

	for _, table := range []string{"repo_graph_gen", "derive_state"} {
		var n int
		require.NoError(t, store.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n))
		require.Equal(t, 1, n, "%s must exist on a fresh store", table)
	}
	var gen int
	require.NoError(t, store.db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('enrichment_state') WHERE name='gen'`).Scan(&gen))
	require.Equal(t, 1, gen)

	// A fresh store has no repos, so the migration must seed nothing.
	var rows int
	require.NoError(t, store.db.QueryRow(`SELECT COUNT(*) FROM derive_state`).Scan(&rows))
	require.Zero(t, rows, "a fresh store must not invent legacy rows")
}
