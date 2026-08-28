package store_sqlite

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// repoSubgraphSidecarTables are the prefix-keyed tables a copy carries under
// the new prefix. They are exactly rekeyMoveTables, for the same reason
// recorded there: every one is keyed by (repo_prefix, file_path) or
// (repo_prefix, provider), never by node_id. Row content is carried verbatim
// except for the columns named in repoSubgraphSidecarPathColumns. The FTS
// corpora are handled separately by copyFTSCorpora, which has to rewrite ids
// and re-map docids.
//
// The id-keyed sidecars and the FTS vtables are deliberately NOT copied.
// Their rows carry the source's node ids, and an FTS5 corpus cannot be
// row-copied under new ids; they rebuild on demand. A freshly copied
// repository is therefore complete in the graph and cold in search.
var repoSubgraphSidecarTables = []string{
	"file_mtimes",
	"files",
	"repo_index_state",
	"enrichment_state",
	"contract_state",
}

// repoSubgraphSidecarPathColumns names, per sidecar table, the columns holding
// a repository-prefixed path — the ones that have to travel with the node ids.
//
// The list is per-table on purpose, because the two conventions look alike and
// are not: `files.file_path` reads "local/his/models/x.py", while
// `file_mtimes.file_path` reads "his/models/x.py" — repo-relative, no prefix.
// Rewriting the latter would prefix every path and break the mtime restat that
// registers the copied checkout.
var repoSubgraphSidecarPathColumns = map[string][]string{
	"files": {"file_path"},
}

// realColumns lists a table's stored columns, skipping generated ones —
// `edges.from_repo` is generated from from_id, so naming it in an INSERT is
// an error and letting SQLite derive it is what keeps it consistent with the
// rewritten id.
func (s *Store) realColumns(table string) ([]string, error) {
	rows, err := s.db.Query(`SELECT name FROM pragma_table_xinfo(?) WHERE hidden IN (0,1) ORDER BY cid`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// CopyRepoSubgraph duplicates srcPrefix's subgraph under dstPrefix.
//
// Every statement is INSERT OR IGNORE: the copy adds rows under the new
// prefix and yields to anything already present, so it cannot modify another
// repository's rows even where the two share a globally-keyed node.
func (s *Store) CopyRepoSubgraph(srcPrefix, dstPrefix string) (graph.RepoSubgraphCopyResult, error) {
	var out graph.RepoSubgraphCopyResult
	if srcPrefix == "" || dstPrefix == "" {
		return out, fmt.Errorf("store_sqlite: CopyRepoSubgraph refuses an empty prefix")
	}
	if srcPrefix == dstPrefix {
		return out, fmt.Errorf("store_sqlite: CopyRepoSubgraph refuses src == dst (%q)", srcPrefix)
	}

	nodeCols, err := s.realColumns("nodes")
	if err != nil {
		return out, fmt.Errorf("store_sqlite: CopyRepoSubgraph nodes columns: %w", err)
	}
	edgeCols, err := s.realColumns("edges")
	if err != nil {
		return out, fmt.Errorf("store_sqlite: CopyRepoSubgraph edges columns: %w", err)
	}

	// Without a unique index on edges there is nothing for INSERT OR IGNORE
	// to dedupe against, so a second copy into the same prefix would double
	// every edge. A copy is only ever meant to populate a fresh prefix.
	occupied, err := s.repoPrefixOccupied(dstPrefix)
	if err != nil {
		return out, err
	}
	if occupied {
		return out, fmt.Errorf(
			"store_sqlite: CopyRepoSubgraph refuses destination %q: it already holds rows", dstPrefix)
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return out, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	idExpr := func(col string) (string, []any) {
		slash, colon := srcPrefix+"/", srcPrefix+"::"
		expr := fmt.Sprintf(
			`CASE WHEN substr(%[1]s,1,%[2]d) = ? THEN ? || substr(%[1]s,%[3]d) `+
				`WHEN substr(%[1]s,1,%[4]d) = ? THEN ? || substr(%[1]s,%[5]d) `+
				`ELSE %[1]s END`,
			col, len(slash), len(slash)+1, len(colon), len(colon)+1)
		return expr, []any{slash, dstPrefix + "/", colon, dstPrefix + "::"}
	}

	// pathExpr rewrites a repository-prefixed *path* column. Paths need their
	// own rewrite and cannot share idExpr: a path only ever carries the
	// "<prefix>/" grammar, never "<prefix>::", and feeding it the synthetic
	// arm would rewrite source text that merely happens to start that way.
	// Anything unmatched — the empty string, a foreign prefix — is left alone.
	//
	// nodes.file_dir is deliberately absent from every caller's path set: it
	// is a VIRTUAL generated column over file_path (fileDirColumnDDL), so
	// realColumns drops it from the projection and SQLite recomputes it from
	// the rewritten path. It is worth knowing that a repository-root file
	// gives file_dir the bare prefix with no separator, because that is the
	// one value an anchor of "<prefix>/" would miss — but it is derived, so
	// there is nothing here to anchor.
	pathExpr := func(col string) (string, []any) {
		slash := srcPrefix + "/"
		expr := fmt.Sprintf(
			`CASE WHEN substr(%[1]s,1,%[2]d) = ? THEN ? || substr(%[1]s,%[3]d) ELSE %[1]s END`,
			col, len(slash), len(slash)+1)
		return expr, []any{slash, dstPrefix + "/"}
	}

	project := func(cols []string, idCols, pathCols map[string]bool, prefixCol string) (string, []any) {
		sel := make([]string, 0, len(cols))
		var args []any
		for _, c := range cols {
			switch {
			case idCols[c]:
				expr, a := idExpr(c)
				sel = append(sel, expr)
				args = append(args, a...)
			case pathCols[c]:
				expr, a := pathExpr(c)
				sel = append(sel, expr)
				args = append(args, a...)
			case c == prefixCol:
				sel = append(sel, "?")
				args = append(args, dstPrefix)
			default:
				sel = append(sel, c)
			}
		}
		return strings.Join(sel, ","), args
	}

	nodeSel, nodeArgs := project(nodeCols,
		map[string]bool{"id": true},
		map[string]bool{"file_path": true},
		"repo_prefix")
	nodeArgs = append(nodeArgs, srcPrefix)
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO nodes (`+strings.Join(nodeCols, ",")+`) SELECT `+
			nodeSel+` FROM nodes WHERE repo_prefix = ?`, nodeArgs...)
	if err != nil {
		return out, fmt.Errorf("store_sqlite: CopyRepoSubgraph nodes: %w", err)
	}
	out.Nodes = rowsAffected(res)

	// from_repo is generated from from_id, so the edge frontier is selected
	// by the source's generated value and lands under the destination's
	// automatically once from_id is rewritten.
	// edges.id is INTEGER PRIMARY KEY AUTOINCREMENT and the table carries no
	// unique index, so the identity of a copied edge is its new rowid.
	// Carrying the source's id over collides on the primary key and INSERT OR
	// IGNORE then silently drops every single row — the copy reports success
	// and moves nothing. Omit the column and let SQLite assign.
	edgeCols = withoutColumn(edgeCols, "id")
	edgeSel, edgeArgs := project(edgeCols,
		map[string]bool{"from_id": true, "to_id": true},
		map[string]bool{"file_path": true},
		"")
	edgeArgs = append(edgeArgs, srcPrefix)
	res, err = tx.Exec(
		`INSERT OR IGNORE INTO edges (`+strings.Join(edgeCols, ",")+`) SELECT `+
			edgeSel+` FROM edges WHERE from_repo = ?`, edgeArgs...)
	if err != nil {
		return out, fmt.Errorf("store_sqlite: CopyRepoSubgraph edges: %w", err)
	}
	out.Edges = rowsAffected(res)

	for _, table := range repoSubgraphSidecarTables {
		cols, err := s.realColumns(table)
		if err != nil || len(cols) == 0 {
			continue // table absent at this schema version
		}
		pathCols := map[string]bool{}
		for _, c := range repoSubgraphSidecarPathColumns[table] {
			pathCols[c] = true
		}
		sel, args := project(cols, nil, pathCols, "repo_prefix")
		args = append(args, srcPrefix)
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO `+table+` (`+strings.Join(cols, ",")+`) SELECT `+
				sel+` FROM `+table+` WHERE repo_prefix = ?`, args...)
		if err != nil {
			return out, fmt.Errorf("store_sqlite: CopyRepoSubgraph %s: %w", table, err)
		}
		out.Sidecars += rowsAffected(res)
	}

	// The search corpora. These are the reason a copied repository would
	// otherwise be complete in the graph and invisible to search: symbol_fts
	// carries the source's node ids, so without a rewrite `search_symbols`
	// returns nothing for the new prefix.
	fts, err := copyFTSCorpora(tx, srcPrefix, dstPrefix, idExpr, pathExpr)
	if err != nil {
		return out, err
	}
	out.Sidecars += fts

	if err := tx.Commit(); err != nil {
		return out, err
	}
	s.markMutationReceiptsIncompleteLocked()
	return out, nil
}

// copyFTSCorpora duplicates the FTS5 search corpora under the new prefix.
//
// Both are plain (not external-content) FTS5 tables, so rows can be inserted
// directly and SQLite assigns fresh docids. The docid is exactly what cannot
// be carried across — which is why a re-key drops these tables rather than
// relabelling them — so each corpus is inserted first and its id-mapping
// table is then rebuilt by reading back the docids SQLite chose.
func copyFTSCorpora(tx txExecer, srcPrefix, dstPrefix string, idExpr, pathExpr func(string) (string, []any)) (int, error) {
	copied := 0

	symExpr, symArgs := idExpr("node_id")
	args := append(append([]any{}, symArgs...), dstPrefix, srcPrefix)
	res, err := tx.Exec(
		`INSERT INTO symbol_fts (node_id, repo_prefix, tokens) `+
			`SELECT `+symExpr+`, ?, tokens FROM symbol_fts WHERE repo_prefix = ?`, args...)
	if err != nil {
		return copied, fmt.Errorf("store_sqlite: CopyRepoSubgraph symbol_fts: %w", err)
	}
	copied += rowsAffected(res)

	// Rebuild the node_id -> docid map from the docids just assigned.
	res, err = tx.Exec(
		`INSERT OR IGNORE INTO symbol_fts_rowid (node_id, repo_prefix, fts_rowid) `+
			`SELECT node_id, repo_prefix, rowid FROM symbol_fts WHERE repo_prefix = ?`, dstPrefix)
	if err != nil {
		return copied, fmt.Errorf("store_sqlite: CopyRepoSubgraph symbol_fts_rowid: %w", err)
	}
	copied += rowsAffected(res)

	// Without the normalization marker the destination reads as un-normalised
	// and the corpus is rebuilt from scratch on the next pass.
	if _, err := tx.Exec(
		`INSERT OR IGNORE INTO symbol_fts_state (repo_prefix, normalization) `+
			`SELECT ?, normalization FROM symbol_fts_state WHERE repo_prefix = ?`,
		dstPrefix, srcPrefix); err != nil {
		return copied, fmt.Errorf("store_sqlite: CopyRepoSubgraph symbol_fts_state: %w", err)
	}

	// content_fts carries a prefixed file_path of its own, and content_fts_rowid
	// is rebuilt below by reading it back — so rewriting it here fixes both.
	bodyExpr, bodyArgs := idExpr("node_id")
	bodyPathExpr, bodyPathArgs := pathExpr("file_path")
	args = append(append([]any{}, bodyArgs...), dstPrefix)
	args = append(args, bodyPathArgs...)
	args = append(args, srcPrefix)
	res, err = tx.Exec(
		`INSERT INTO content_fts (node_id, repo_prefix, file_path, ordinal, body) `+
			`SELECT `+bodyExpr+`, ?, `+bodyPathExpr+`, ordinal, body FROM content_fts WHERE repo_prefix = ?`, args...)
	if err != nil {
		return copied, fmt.Errorf("store_sqlite: CopyRepoSubgraph content_fts: %w", err)
	}
	copied += rowsAffected(res)

	res, err = tx.Exec(
		`INSERT OR IGNORE INTO content_fts_rowid (fts_rowid, repo_prefix, file_path) `+
			`SELECT rowid, repo_prefix, file_path FROM content_fts WHERE repo_prefix = ?`, dstPrefix)
	if err != nil {
		return copied, fmt.Errorf("store_sqlite: CopyRepoSubgraph content_fts_rowid: %w", err)
	}
	copied += rowsAffected(res)

	return copied, nil
}

// txExecer is the slice of *sql.Tx copyFTSCorpora needs.
type txExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func rowsAffected(res sql.Result) int {
	n, err := res.RowsAffected()
	if err != nil {
		return 0
	}
	return int(n)
}

// withoutColumn drops one column name from a projection list.
func withoutColumn(cols []string, drop string) []string {
	out := make([]string, 0, len(cols))
	for _, c := range cols {
		if c != drop {
			out = append(out, c)
		}
	}
	return out
}

// repoPrefixOccupied reports whether a prefix already owns graph rows.
func (s *Store) repoPrefixOccupied(prefix string) (bool, error) {
	var n int
	err := s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM nodes WHERE repo_prefix = ? LIMIT 1)`, prefix).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store_sqlite: CopyRepoSubgraph destination probe: %w", err)
	}
	if n != 0 {
		return true, nil
	}
	err = s.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM edges WHERE from_repo = ? LIMIT 1)`, prefix).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store_sqlite: CopyRepoSubgraph destination probe: %w", err)
	}
	return n != 0, nil
}
