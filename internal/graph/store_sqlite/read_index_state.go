package store_sqlite

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// ReadRepoIndexStates opens the SQLite store at path read-only and returns
// every repo_index_state freshness row keyed by repo_prefix.
//
// It is a deliberately lightweight side door for read-only callers that must
// inspect index freshness WITHOUT going through Open — see openReadOnlyStore
// for why, and for the pragma choice this shares with every other read-only
// reader.
//
// Exactly two conditions mean "nothing recorded yet" and yield an empty map
// with a nil error: no store file at all, and a store that predates the
// repo_index_state table. Every other failure — a corrupt or truncated
// database, a permission error, a schema the query cannot run against — is
// returned. Collapsing those into an empty map made a broken store
// indistinguishable from an unindexed one, so `gortex repos` reported success
// and told the user their repos had never been indexed.
func ReadRepoIndexStates(path string) (map[string]graph.RepoIndexState, error) {
	db, found, err := openReadOnlyStore(path)
	if err != nil {
		return nil, err
	}
	if !found {
		return map[string]graph.RepoIndexState{}, nil
	}
	defer db.Close() //nolint:errcheck // read-only handle
	return scanIndexStates(db, path)
}

// scanIndexStates reads every repo_index_state row from an already-open
// read-only handle, so the single-table reader and the combined readiness
// reader cannot drift in what "fresh" is read from.
func scanIndexStates(db *sql.DB, path string) (map[string]graph.RepoIndexState, error) {
	rows, err := db.Query(`
SELECT repo_prefix, indexed_sha, dirty, indexed_at, workspace_fp, node_count, edge_count, extractor_versions
  FROM repo_index_state`)
	if err != nil {
		// A store written before the repo_index_state table existed has
		// nothing recorded yet. Anything else — most importantly a file that
		// is not a database — is a real failure the caller must see.
		if isMissingTableErr(err) {
			return map[string]graph.RepoIndexState{}, nil
		}
		return nil, fmt.Errorf("read repo_index_state from %q: %w", path, err)
	}
	defer rows.Close() //nolint:errcheck // read-only cursor

	out := map[string]graph.RepoIndexState{}
	for rows.Next() {
		var st graph.RepoIndexState
		var dirty int
		if err := rows.Scan(&st.RepoPrefix, &st.IndexedSHA, &dirty, &st.IndexedAt,
			&st.WorkspaceFP, &st.NodeCount, &st.EdgeCount, &st.ExtractorVersions); err != nil {
			return nil, fmt.Errorf("scan repo_index_state: %w", err)
		}
		st.Dirty = dirty != 0
		out[st.RepoPrefix] = st
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repo_index_state: %w", err)
	}
	return out, nil
}

// isMissingTableErr reports whether err is SQLite refusing a query because the
// table does not exist. String-matched because the driver reports it as a
// generic "SQL logic error" with the detail only in the message; the query
// above names exactly one table, so this cannot mistake some other absence for
// it. A corrupt file reports "file is not a database" and is deliberately not
// covered here.
func isMissingTableErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}
