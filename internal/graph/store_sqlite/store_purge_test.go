package store_sqlite

import (
	"database/sql"
	"github.com/zzet/gortex/internal/graph"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openPurgeStore opens a throwaway on-disk store for the hygiene tests.
func openPurgeStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "purge.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// seedRepoRows inserts exactly one row keyed to prefix into nodes, edges,
// vectors, and every repo_prefix-keyed sidecar table, so a PurgeRepo /
// RekeyRepoPrefix test can assert each table is cleared/moved. Node/vector
// ids embed the prefix (`<prefix>::a.go::X`) so the node-id-keyed vectors +
// edges land in the prefix's scope. Uses raw SQL for exhaustiveness: some
// tables have no public setter.
func seedRepoRows(t *testing.T, db *sql.DB, prefix string) {
	t.Helper()
	nodeID := prefix + "::a.go::X"
	chunkID := nodeID + "#chunk0"
	exec := func(q string, args ...any) {
		t.Helper()
		_, err := db.Exec(q, args...)
		require.NoError(t, err, q)
	}
	exec(`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES (?, 'function', 'X', 'a.go', ?)`, nodeID, prefix)
	// An edge from the repo's node to a shared '' global external: PurgeRepo
	// must delete this edge (its from_id is a repo node) but NEVER the ''
	// target node.
	exec(`INSERT INTO edges (from_id, to_id, kind) VALUES (?, 'external_call::dep:shared', 'calls')`, nodeID)
	exec(`INSERT INTO vectors (node_id, repo_prefix, parent_id, dims, vec) VALUES (?, ?, '', 1, X'00')`, nodeID, prefix)
	exec(`INSERT INTO vectors (node_id, repo_prefix, parent_id, dims, vec) VALUES (?, ?, ?, 1, X'00')`, chunkID, prefix, nodeID)

	exec(`INSERT INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES (?, 'a.go', 123)`, prefix)
	exec(`INSERT INTO repo_index_state (repo_prefix, indexed_sha) VALUES (?, 'sha')`, prefix)
	exec(`INSERT INTO enrichment_state (repo_prefix, provider) VALUES (?, 'lsp')`, prefix)
	exec(`INSERT INTO clone_shingles (node_id, repo_prefix, shingles) VALUES (?, ?, X'00')`, nodeID, prefix)
	exec(`INSERT INTO constant_values (node_id, repo_prefix, file_path, value) VALUES (?, ?, 'a.go', 'v')`, nodeID, prefix)
	exec(`INSERT INTO files (repo_prefix, file_path, content_hash) VALUES (?, 'a.go', 'h')`, prefix)
	exec(`INSERT INTO ref_facts (repo_prefix, from_id, to_id, kind, line) VALUES (?, ?, 'a.go::Y', 'ref', 1)`, prefix, nodeID)
	exec(`INSERT INTO churn_enrichment (node_id, repo_prefix, commit_count) VALUES (?, ?, 3)`, nodeID, prefix)
	exec(`INSERT INTO coverage_enrichment (node_id, repo_prefix, coverage_pct) VALUES (?, ?, 0.5)`, nodeID, prefix)
	exec(`INSERT INTO release_enrichment (node_id, repo_prefix, added_in) VALUES (?, ?, 'v1')`, nodeID, prefix)
	exec(`INSERT INTO blame_enrichment (node_id, repo_prefix, email) VALUES (?, ?, 'a@b')`, nodeID, prefix)
	exec(`INSERT INTO symbol_fts (node_id, repo_prefix, tokens) VALUES (?, ?, 'x')`, nodeID, prefix)
	var symbolRowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM symbol_fts WHERE node_id = ?`, nodeID).Scan(&symbolRowid))
	exec(`INSERT INTO symbol_fts_rowid (node_id, repo_prefix, fts_rowid) VALUES (?, ?, ?)`, nodeID, prefix, symbolRowid)
	exec(`INSERT INTO content_fts (node_id, repo_prefix, file_path, ordinal, body) VALUES (?, ?, 'a.go', 0, 'body')`, nodeID, prefix)
	var contentRowid int64
	require.NoError(t, db.QueryRow(`SELECT rowid FROM content_fts WHERE node_id = ?`, nodeID).Scan(&contentRowid))
	exec(`INSERT INTO content_fts_rowid (fts_rowid, repo_prefix, file_path) VALUES (?, ?, 'a.go')`, contentRowid, prefix)
}

// countByPrefix reports how many rows a repo_prefix-keyed table holds for
// prefix. nodes and every sidecar carry a repo_prefix column.
func countByPrefix(t *testing.T, db *sql.DB, table, prefix string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE repo_prefix = ?`, prefix).Scan(&n))
	return n
}

// prefixKeyedTables is every repo_prefix-keyed table PurgeRepo/Rekey touch,
// minus nodes (asserted separately) — used to loop assertions.
var prefixKeyedTables = []string{
	"file_mtimes", "repo_index_state", "enrichment_state", "clone_shingles",
	"constant_values", "files", "ref_facts", "churn_enrichment",
	"coverage_enrichment", "release_enrichment", "blame_enrichment",
	"symbol_fts", "symbol_fts_rowid", "content_fts", "content_fts_rowid",
}

func TestPurgeRepo_ClearsEveryTable_LeavesOthersAndGlobals(t *testing.T) {
	s := openPurgeStore(t)
	// Two real repos plus a shared '' global-external node the purge must
	// never touch.
	seedRepoRows(t, s.writerDB, "repoA")
	seedRepoRows(t, s.writerDB, "repoB")
	_, err := s.writerDB.Exec(`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES ('external_call::dep:shared', 'external', 'shared', '', '')`)
	require.NoError(t, err)

	require.NoError(t, s.PurgeRepo("repoA"))

	// repoA: nodes, edges, vectors, and every sidecar cleared.
	assert.Equal(t, 0, countByPrefix(t, s.db, "nodes", "repoA"), "repoA nodes gone")
	assert.Equal(t, 0, countByPrefix(t, s.db, "vectors", "repoA"), "repoA ordinary and chunk vectors gone")
	for _, tbl := range prefixKeyedTables {
		assert.Equal(t, 0, countByPrefix(t, s.db, tbl, "repoA"), "repoA %s cleared", tbl)
	}
	var edgesFromA int
	require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM edges WHERE from_id LIKE 'repoA::%'`).Scan(&edgesFromA))
	assert.Equal(t, 0, edgesFromA, "repoA edges gone")

	// repoB untouched across the board.
	assert.Equal(t, 1, countByPrefix(t, s.db, "nodes", "repoB"), "repoB nodes intact")
	assert.Equal(t, 2, countByPrefix(t, s.db, "vectors", "repoB"), "repoB ordinary and chunk vectors intact")
	for _, tbl := range prefixKeyedTables {
		assert.Equal(t, 1, countByPrefix(t, s.db, tbl, "repoB"), "repoB %s intact", tbl)
	}

	// The shared '' global external survives — nothing may purge ''.
	assert.Equal(t, 1, countByPrefix(t, s.db, "nodes", ""), "'' global external survives")
}

func TestPurgeRepo_RefusesEmptyPrefix(t *testing.T) {
	s := openPurgeStore(t)
	seedRepoRows(t, s.writerDB, "")
	require.Error(t, s.PurgeRepo(""), "PurgeRepo must refuse the empty prefix (global externals / solo data)")
	// The '' rows are still there.
	assert.Equal(t, 1, countByPrefix(t, s.db, "file_mtimes", ""), "'' file_mtimes untouched by refused purge")
}

func TestOrphanRepoPrefixes_SidecarOnlyResidue(t *testing.T) {
	s := openPurgeStore(t)
	// gone: a repo whose NODES were evicted but whose sidecars linger — the
	// exact leaked-untrack shape (residue in file_mtimes + repo_index_state,
	// no nodes). live: a fully-present tracked repo.
	_, err := s.writerDB.Exec(`INSERT INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES ('gone', 'x.go', 1)`)
	require.NoError(t, err)
	_, err = s.writerDB.Exec(`INSERT INTO repo_index_state (repo_prefix) VALUES ('gone')`)
	require.NoError(t, err)
	// A vector-only legacy leak must also be discoverable even when every graph
	// node and other sidecar for the repository is gone.
	_, err = s.writerDB.Exec(`INSERT INTO vectors (node_id, repo_prefix, parent_id, dims, vec) VALUES ('vector-only::ghost#chunk0', 'vector-only', 'vector-only::ghost', 1, X'00')`)
	require.NoError(t, err)
	seedRepoRows(t, s.writerDB, "live")
	// A '' row must never be reported as an orphan.
	_, err = s.writerDB.Exec(`INSERT INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES ('', 'g.go', 1)`)
	require.NoError(t, err)

	orphans := s.OrphanRepoPrefixes([]string{"live"})
	assert.ElementsMatch(t, []string{"gone", "vector-only"}, orphans,
		"both sidecar-only and vector-only residue must be reported")

	// Case-fold safety net: a case-only spelling drift of a tracked repo is
	// NOT an orphan.
	assert.Empty(t, s.OrphanRepoPrefixes([]string{"LIVE", "GONE", "VECTOR-ONLY"}),
		"case-insensitive known set covers every prefix")
}

func TestRekeyRepoPrefix_MovesProvenanceDropsNodeIDKeyed(t *testing.T) {
	s := openPurgeStore(t)
	seedRepoRows(t, s.writerDB, "") // solo repo: everything under ''

	require.NoError(t, s.RekeyRepoPrefix("", "drools"))

	// Prefix/path-keyed provenance MOVED '' -> drools (so warm restart finds
	// the repo's mtimes under the new prefix instead of full-re-tracking).
	moveTables := []string{"file_mtimes", "files", "repo_index_state", "enrichment_state"}
	for _, tbl := range moveTables {
		assert.Equal(t, 0, countByPrefix(t, s.db, tbl, ""), "%s '' rows moved out", tbl)
		assert.Equal(t, 1, countByPrefix(t, s.db, tbl, "drools"), "%s rows now under new prefix", tbl)
	}

	// node_id-keyed tables DROPPED (their old ids are dangling after the
	// re-mint) — the FTS decision included.
	dropTables := []string{
		"clone_shingles", "constant_values", "ref_facts", "churn_enrichment",
		"coverage_enrichment", "release_enrichment", "blame_enrichment",
		"symbol_fts", "symbol_fts_rowid", "content_fts", "content_fts_rowid",
	}
	for _, tbl := range dropTables {
		assert.Equal(t, 0, countByPrefix(t, s.db, tbl, ""), "%s '' rows dropped", tbl)
		assert.Equal(t, 0, countByPrefix(t, s.db, tbl, "drools"), "%s NOT relabeled to new prefix", tbl)
	}
	assert.Equal(t, 0, countByPrefix(t, s.db, "vectors", ""), "old-prefix vectors dropped")
	assert.Equal(t, 0, countByPrefix(t, s.db, "vectors", "drools"), "vectors are not relabeled to new IDs")

	assert.Error(t, s.RekeyRepoPrefix("repoA", ""), "rekey INTO the empty prefix is refused")
}

// TestContentCrashWindow simulates the D4 kill-window at the store level:
// per-file delete+append leaves a mix of old+new content instead of an empty
// table, and the end-of-track sweep (keep = files that STREAMED content this
// run) reaps both files that vanished from disk and files that still exist
// but no longer yield content sections.
func TestContentCrashWindow(t *testing.T) {
	s := openPurgeStore(t)
	item := func(id, file, body string) graph.ContentFTSItem {
		return graph.ContentFTSItem{NodeID: id, FilePath: file, Ordinal: 0, Body: body}
	}
	// Prior full index: three content files present.
	require.NoError(t, s.AppendContent("r", []graph.ContentFTSItem{item("r::f1", "f1.md", "old one")}))
	require.NoError(t, s.AppendContent("r", []graph.ContentFTSItem{item("r::f2", "f2.md", "old two")}))
	require.NoError(t, s.AppendContent("r", []graph.ContentFTSItem{item("r::f3", "f3.md", "old three")}))

	// A new full index re-streams only f1 (crash before it reached the
	// rest): delete f1's rows then re-append. The other files' OLD rows must
	// survive — no empty-table window.
	require.NoError(t, s.WipeContentFileInRepo("r", "f1.md"))
	require.NoError(t, s.AppendContent("r", []graph.ContentFTSItem{item("r::f1", "f1.md", "new one")}))

	countFile := func(file string) int {
		var n int
		require.NoError(t, s.db.QueryRow(`SELECT COUNT(*) FROM content_fts WHERE repo_prefix='r' AND file_path=?`, file).Scan(&n))
		return n
	}
	assert.Equal(t, 1, countFile("f1.md"), "f1 refreshed (delete+append, not doubled)")
	assert.Equal(t, 1, countFile("f2.md"), "f2's old rows survive the mid-parse kill (no empty table)")
	assert.Equal(t, 1, countFile("f3.md"), "f3's old rows survive the mid-parse kill (no empty table)")

	// The next SUCCESSFUL completion: f2 was deleted from the repo, and f3
	// STILL EXISTS on disk but was emptied — it streamed no content sections
	// this run, so it is absent from the streamed set. The sweep keeps
	// exactly the streamed set {f1}, reaping both the vanished file and the
	// content->no-content transition (a disk-survival keep would have
	// protected f3's stale rows forever).
	require.NoError(t, s.DeleteContentFilesForRepoNotIn("r", map[string]struct{}{"f1.md": {}}))
	assert.Equal(t, 1, countFile("f1.md"), "still-streaming file kept")
	assert.Equal(t, 0, countFile("f2.md"), "vanished file swept")
	assert.Equal(t, 0, countFile("f3.md"), "content->no-content transition swept despite surviving on disk")

	// Empty keep is a deliberate no-op (the never-wipe-from-empty safety
	// net; a zero-content walk routes to WipeContent instead).
	require.NoError(t, s.DeleteContentFilesForRepoNotIn("r", nil))
	assert.Equal(t, 1, countFile("f1.md"), "empty keep never wipes")
}
