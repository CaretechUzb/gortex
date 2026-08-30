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
// repo_graph_gen and derive_state ride along so a copied checkout does not read
// "never derived" — the state readiness reserves for a repo whose queries
// really do return a subset. Their values are then re-asserted for the
// destination once its own file bookkeeping lands; see RestampCopiedReadiness.
// They are deliberately NOT in orphanScanTables: a repo_graph_gen row can
// legitimately exist before any node does (a cold index bumps the anchor before
// it stamps repo_index_state), so scanning them for orphans would name a repo
// that is merely mid-index.
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
	"repo_graph_gen",
	"derive_state",
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

	// The frontier is the two id key ranges, NOT `from_repo = ?`. from_repo is
	// GENERATED from the first '/' in from_id, so it only understands the
	// `<prefix>/` grammar: a synthetic `<prefix>::…` id has no slash at all and
	// generates the empty string, while one that happens to carry a slash
	// deeper in — `local::builtin::js::array/map/object::entries` — generates a
	// garbage prefix. Either way `from_repo = 'local'` matches none of them, and
	// every edge SOURCED at a synthetic node is silently left behind: 245
	// member_of edges binding stdlib symbols to their module on the measured
	// workspace, against 254 in the derived checkout beside it. This is the
	// same frontier copyInboundEdges uses, and it excludes sibling checkouts by
	// construction rather than by luck.
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
	edgeArgs = append(edgeArgs, prefixKeyRanges(srcPrefix)...)
	res, err = tx.Exec(
		`INSERT OR IGNORE INTO edges (`+strings.Join(edgeCols, ",")+`) SELECT `+
			edgeSel+` FROM edges WHERE (from_id >= ? AND from_id < ?) OR (from_id >= ? AND from_id < ?)`,
		edgeArgs...)
	if err != nil {
		return out, fmt.Errorf("store_sqlite: CopyRepoSubgraph edges: %w", err)
	}
	out.Edges = rowsAffected(res)

	inbound, err := s.copyInboundEdges(tx, srcPrefix, dstPrefix, edgeCols, idExpr)
	if err != nil {
		return out, err
	}
	out.InboundEdges = inbound

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

// prefixKeyRanges returns the two half-open key ranges that select every node
// id under a prefix, in the form the edges_by_to index can seek rather than
// the substr() form idExpr uses for rewriting. '/'+1 is '0' and ':'+1 is ';',
// so each id grammar is exactly one range, and both exclude a sibling
// checkout: '@' sorts above both terminators.
func prefixKeyRanges(prefix string) []any {
	return []any{prefix + "/", prefix + "0", prefix + "::", prefix + ";"}
}

// copyInboundEdges carries the edges OTHER repositories mint into srcPrefix
// across to dstPrefix. Only to_id moves: from_id stays with its owner, and so
// does file_path, which names a file in the *source* repository of the edge.
//
// These edges are invisible to the outbound copy because a global pass owns an
// edge by its source node, and they are not a rounding error — on the measured
// Odoo workspace a derived checkout carries 185,023 of them (110,865 from
// `odoo`, 74,149 from `addons`), a fifth of everything touching it. Without
// them the copy answers "what does this reference" perfectly and "who uses
// this" with silence.
//
// That these belong in a copy at all was measured, not assumed: across the two
// derived checkouts of one repository, 14,067 `odoo` symbols bind into `local`
// and 13,960 into `local@aurora-redesign` — 13,954 into BOTH. The binder fans
// out to every checkout, so a derive that saw the destination would mint these
// same edges. The copy anticipates it; it does not invent state a derive would
// contradict.
//
// A sibling checkout must never be a source. Two checkouts of one repository
// binding to each other is the exact contamination checkout groups exist to
// prevent, and today no such edge exists to copy — but the filter is what
// keeps that true rather than lucky.
func (s *Store) copyInboundEdges(
	tx txExecer, srcPrefix, dstPrefix string, edgeCols []string, idExpr func(string) (string, []any),
) (int, error) {
	ranges := prefixKeyRanges(srcPrefix)
	rows, err := s.db.Query(
		`SELECT DISTINCT from_repo FROM edges `+
			`WHERE ((to_id >= ? AND to_id < ?) OR (to_id >= ? AND to_id < ?)) AND from_repo <> ''`,
		ranges...)
	if err != nil {
		return 0, fmt.Errorf("store_sqlite: CopyRepoSubgraph inbound sources: %w", err)
	}
	var sources []any
	for rows.Next() {
		var repo string
		if err := rows.Scan(&repo); err != nil {
			rows.Close()
			return 0, err
		}
		if repo == srcPrefix || repo == dstPrefix || graph.SiblingCheckouts(s, srcPrefix, repo) {
			continue
		}
		sources = append(sources, repo)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(sources) == 0 {
		return 0, nil
	}

	sel, args := projectEdgeInbound(edgeCols, idExpr)
	args = append(args, ranges...)
	args = append(args, sources...)
	res, err := tx.Exec(
		`INSERT OR IGNORE INTO edges (`+strings.Join(edgeCols, ",")+`) SELECT `+sel+
			` FROM edges WHERE ((to_id >= ? AND to_id < ?) OR (to_id >= ? AND to_id < ?)) AND from_repo IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(sources)), ",")+`)`, args...)
	if err != nil {
		return 0, fmt.Errorf("store_sqlite: CopyRepoSubgraph inbound edges: %w", err)
	}
	return rowsAffected(res), nil
}

// projectEdgeInbound is the outbound projection's mirror: to_id is rewritten
// and every other column, from_id and file_path included, is carried verbatim.
func projectEdgeInbound(cols []string, idExpr func(string) (string, []any)) (string, []any) {
	sel := make([]string, 0, len(cols))
	var args []any
	for _, c := range cols {
		if c == "to_id" {
			expr, a := idExpr(c)
			sel = append(sel, expr)
			args = append(args, a...)
			continue
		}
		sel = append(sel, c)
	}
	return strings.Join(sel, ","), args
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

// RestampCopiedReadiness declares the carried stage stamps current for a
// destination checkout, AFTER its own file bookkeeping has been written.
//
// This is a semantic assertion, not metadata tidying. The copy carries the
// source's derive_state and enrichment_state rows verbatim along with its
// counters, so at the instant the copy commits the two already agree and there
// is nothing to do. What breaks them apart comes next: registering the copied
// checkout restats its files and calls ReplaceFileMtimes, and a worktree's
// on-disk mtimes differ from the source's even at the identical commit. That
// spurious content change advances the destination's content_gen past every
// carried stamp, and a copied worktree would then read "partial" from the
// moment it lands and stay there permanently — nothing re-derives it, because
// avoiding that derive is the entire reason the copy exists.
//
// So it must run after registration, not inside the copy transaction. The cost
// is a window: a crash between the two leaves the destination reading partial.
// That is a false alarm, which is the safe direction, and re-tracking clears it.
//
// What makes the assertion true is the exact-copy invariant: the destination
// holds the same nodes and edges as the source, at the same commit, so the
// derived and enriched edges carried across describe it exactly as well as they
// describe the source. If that invariant ever has a blind spot, this re-stamp
// hides it — which is why it is tied to a test that asserts the invariant
// rather than the re-stamp.
//
// Rows still at content_gen 0 are left alone: those are providers declared
// applicable that have never run, and laundering them into current would be the
// one thing the applicability model exists to prevent.
func (s *Store) RestampCopiedReadiness(dstPrefix string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op
	if err := restampCopiedReadiness(tx, dstPrefix); err != nil {
		return err
	}
	return tx.Commit()
}

func restampCopiedReadiness(tx *sql.Tx, dstPrefix string) error {
	for _, stmt := range []string{
		`UPDATE derive_state
		    SET derived_content_gen = COALESCE(
		          (SELECT content_gen FROM repo_graph_gen WHERE repo_prefix = ?1), 0),
		        derived_gen         = COALESCE(
		          (SELECT gen         FROM repo_graph_gen WHERE repo_prefix = ?1), 0)
		  WHERE repo_prefix = ?1 AND legacy = 0`,
		`UPDATE enrichment_state
		    SET content_gen = COALESCE(
		          (SELECT content_gen FROM repo_graph_gen WHERE repo_prefix = ?1), 0),
		        gen         = COALESCE(
		          (SELECT gen         FROM repo_graph_gen WHERE repo_prefix = ?1), 0)
		  WHERE repo_prefix = ?1 AND content_gen > 0`,
	} {
		if _, err := tx.Exec(stmt, dstPrefix); err != nil {
			return fmt.Errorf("store_sqlite: CopyRepoSubgraph re-stamp readiness: %w", err)
		}
	}
	return nil
}
