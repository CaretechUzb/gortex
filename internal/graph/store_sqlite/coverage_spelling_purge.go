package store_sqlite

import (
	"database/sql"
	"strings"
)

// Coverage-domain artifact kinds, split by ownership.
//
//   - per-file artifacts belong to exactly one file: the file's spelling IS
//     their identity, so a legacy-spelled row is unambiguously garbage.
//   - shared targets (a license, a team, a module) are referenced by many
//     files. Their FilePath is a first-sighting breadcrumb, never an
//     ownership claim, so they are NEVER selected by path — they leave only
//     when the purge has removed their last reference.
const (
	coveragePerFileNodeKinds = `'todo','fixture'`
	coverageEdgeKinds        = `'annotated','licensed_as','owns','generated_by','depends_on_module'`
)

// purgeLegacyCoverageSpellings removes the coverage-domain rows that
// pre-fix binaries minted under a re-spelled file path.
//
// Until the builders preserved the extractor's spelling, todos / licenses /
// ownership / codegen / fixtures / modules ran their relPath through
// filepath.ToSlash before minting node IDs, FilePath fields, and edge
// endpoints. Everything else in the pipeline keys file identity by the
// indexer's exact spelling — OS-native separators below the repo prefix —
// so on Windows these rows are invisible to eviction, which matches nodes by
// file_path and edges by evicted-endpoint touch. They are never swept, never
// replaced, and never re-created in the new spelling until their file is
// re-parsed, so an upgraded store keeps serving stale TODO text and dangling
// licensed_as / owns / generated_by / depends_on_module edges indefinitely —
// including for files nothing touches again.
//
// Scope, deliberately narrow in three directions:
//
//  1. Store-level guard. The purge runs only when the store holds at least
//     one backslash-bearing file_path, i.e. it was written by a Windows
//     indexer. On a store written on POSIX every path IS the native
//     spelling and the whole migration is a no-op.
//  2. Path-level predicate. Below the repo prefix a Windows-written store
//     spells separators with a backslash, so a forward slash there marks a
//     row no current builder could have produced. Top-level files (no
//     separator below the prefix) were never damaged and never match.
//  3. Kind-level predicate. Only the six coverage domains' own kinds are
//     considered. A shared target is removed only after the edge purge
//     leaves it with no references at all — never because a purged file
//     happened to be its first sighting.
//
// Idempotent: a second run finds no legacy-spelled rows. Bounded to the
// coverage kinds: language-extractor nodes and edges are never candidates.
func purgeLegacyCoverageSpellings(tx *sql.Tx) error {
	windowsWritten, err := storeHasNativeBackslashPaths(tx)
	if err != nil || !windowsWritten {
		return err
	}
	prefixes, err := storeRepoPrefixes(tx)
	if err != nil {
		return err
	}
	legacyNodePath := legacyPathPredicate("nodes.file_path", prefixes)
	legacyEdgePath := legacyPathPredicate("edges.file_path", prefixes)

	// Per-file artifacts whose own spelling is legacy. Collected before any
	// delete so the edge sweep below can clear their endpoints too. The
	// DROPs make the step re-entrant on a connection that carried a temp
	// table over from an earlier attempt.
	if _, err := tx.Exec(`DROP TABLE IF EXISTS covdom_doomed_nodes`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE covdom_doomed_nodes AS
		SELECT id FROM nodes
		WHERE kind IN (` + coveragePerFileNodeKinds + `) AND ` + legacyNodePath); err != nil {
		return err
	}
	defer func() { _, _ = tx.Exec(`DROP TABLE IF EXISTS covdom_doomed_nodes`) }()

	// Shared targets the legacy edges point at. Snapshotted BEFORE the edge
	// delete, because afterwards nothing links them to this purge.
	if _, err := tx.Exec(`DROP TABLE IF EXISTS covdom_shared_targets`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE covdom_shared_targets AS
		SELECT DISTINCT to_id AS id FROM edges
		WHERE kind IN ('licensed_as','generated_by','depends_on_module') AND ` + legacyEdgePath + `
		UNION
		SELECT DISTINCT from_id AS id FROM edges
		WHERE kind = 'owns' AND ` + legacyEdgePath); err != nil {
		return err
	}
	defer func() { _, _ = tx.Exec(`DROP TABLE IF EXISTS covdom_shared_targets`) }()

	// Legacy coverage edges, selected by kind AND their own FilePath
	// spelling — never by touching an evicted endpoint, which would take
	// a shared target's other, still-valid edges with it.
	if _, err := tx.Exec(`DELETE FROM edges
		WHERE kind IN (` + coverageEdgeKinds + `) AND ` + legacyEdgePath); err != nil {
		return err
	}
	// Any remaining edge on a doomed per-file artifact: its node is going,
	// so leaving the edge would strand a dangling endpoint. Safe here (and
	// only here) because these nodes are owned by exactly one file. Two
	// statements rather than one OR: each side then seeks through its own
	// endpoint index instead of scanning the edge table.
	for _, column := range []string{"from_id", "to_id"} {
		if _, err := tx.Exec(`DELETE FROM edges
			WHERE ` + column + ` IN (SELECT id FROM covdom_doomed_nodes)`); err != nil {
			return err
		}
	}

	// A shared target joins the doomed set only once the purge has left it
	// with no references at all. The two NOT EXISTS probes are likewise
	// kept apart so each rides an endpoint index.
	if _, err := tx.Exec(`INSERT INTO covdom_doomed_nodes(id)
		SELECT t.id FROM covdom_shared_targets t
		WHERE EXISTS (SELECT 1 FROM nodes n WHERE n.id = t.id)
		  AND t.id NOT IN (SELECT id FROM covdom_doomed_nodes)
		  AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.from_id = t.id)
		  AND NOT EXISTS (SELECT 1 FROM edges e WHERE e.to_id = t.id)`); err != nil {
		return err
	}

	// Symbol FTS rows outlive their node unless deleted explicitly (see
	// BatchDeleteSymbolFTS, the eviction lane's equivalent) — a purged todo
	// would otherwise keep answering searches with its stale text.
	if _, err := tx.Exec(`DELETE FROM symbol_fts WHERE rowid IN (
		SELECT fts_rowid FROM symbol_fts_rowid
		WHERE node_id IN (SELECT id FROM covdom_doomed_nodes))`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM symbol_fts_rowid
		WHERE node_id IN (SELECT id FROM covdom_doomed_nodes)`); err != nil {
		return err
	}
	_, err = tx.Exec(`DELETE FROM nodes WHERE id IN (SELECT id FROM covdom_doomed_nodes)`)
	return err
}

// storeHasNativeBackslashPaths reports whether any node path carries a
// backslash, i.e. the store was written by an indexer on a platform whose
// separator is not '/'. It is the migration's outer guard: on a store
// written on POSIX no path can be a re-spelled twin of another, so the
// purge must not run at all.
func storeHasNativeBackslashPaths(tx *sql.Tx) (bool, error) {
	var present int
	err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM nodes WHERE instr(file_path, '\') > 0)`).Scan(&present)
	return present == 1, err
}

// storeRepoPrefixes returns the distinct repository prefixes the store's
// nodes carry. Multi-repo IDs and paths are `<repo>/<path>`: that single
// separator is always a forward slash regardless of platform, so it must be
// stripped before a path is judged on its remaining separators.
func storeRepoPrefixes(tx *sql.Tx) ([]string, error) {
	rows, err := tx.Query(`SELECT DISTINCT repo_prefix FROM nodes WHERE repo_prefix <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	var prefixes []string
	for rows.Next() {
		var prefix string
		if err := rows.Scan(&prefix); err != nil {
			return nil, err
		}
		prefixes = append(prefixes, prefix)
	}
	return prefixes, rows.Err()
}

// legacyPathPredicate builds the SQL test "this path is a pre-fix
// re-spelling": strip the `<repo>/` prefix when one applies, then look for a
// forward slash in what remains. On a Windows-written store (the only place
// this runs — see storeHasNativeBackslashPaths) the remainder's separators
// are backslashes, so a forward slash there cannot come from a current
// builder.
//
// Prefixes are embedded as escaped literals rather than bound parameters
// because the predicate is spliced into CREATE TEMP TABLE ... AS SELECT
// statements. The values are the store's own repo_prefix column, and
// quoteSQLLiteral doubles any embedded quote. Exact string comparison, not
// LIKE, so a prefix containing '%' or '_' cannot match a sibling repo.
func legacyPathPredicate(column string, prefixes []string) string {
	// A single-repo store carries no prefix: the whole path is the portion
	// under test. (SQL CASE requires at least one WHEN, so this branch is
	// not merely an optimisation.)
	if len(prefixes) == 0 {
		return "instr(" + column + ", '/') > 0"
	}
	var b strings.Builder
	b.WriteString("instr(CASE")
	for _, prefix := range prefixes {
		lit := quoteSQLLiteral(prefix + "/")
		b.WriteString(" WHEN substr(" + column + ", 1, length(" + lit + ")) = " + lit +
			" THEN substr(" + column + ", length(" + lit + ") + 1)")
	}
	b.WriteString(" ELSE " + column + " END, '/') > 0")
	return b.String()
}

// quoteSQLLiteral renders s as a single-quoted SQLite string literal.
func quoteSQLLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
