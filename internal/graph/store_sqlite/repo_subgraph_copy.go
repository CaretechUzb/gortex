package store_sqlite

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// repoSubgraphSidecarTables are the prefix-keyed tables whose row content is
// independent of node ids, so a copy carries them verbatim under the new
// prefix. They are exactly rekeyMoveTables, for the same reason recorded
// there: every one is keyed by (repo_prefix, file_path) or (repo_prefix,
// provider), never by node_id.
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

	project := func(cols []string, idCols map[string]bool, prefixCol string) (string, []any) {
		sel := make([]string, 0, len(cols))
		var args []any
		for _, c := range cols {
			switch {
			case idCols[c]:
				expr, a := idExpr(c)
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

	nodeSel, nodeArgs := project(nodeCols, map[string]bool{"id": true}, "repo_prefix")
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
	edgeSel, edgeArgs := project(edgeCols, map[string]bool{"from_id": true, "to_id": true}, "")
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
		sel, args := project(cols, nil, "repo_prefix")
		args = append(args, srcPrefix)
		res, err := tx.Exec(
			`INSERT OR IGNORE INTO `+table+` (`+strings.Join(cols, ",")+`) SELECT `+
				sel+` FROM `+table+` WHERE repo_prefix = ?`, args...)
		if err != nil {
			return out, fmt.Errorf("store_sqlite: CopyRepoSubgraph %s: %w", table, err)
		}
		out.Sidecars += rowsAffected(res)
	}

	if err := tx.Commit(); err != nil {
		return out, err
	}
	s.markMutationReceiptsIncompleteLocked()
	return out, nil
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
