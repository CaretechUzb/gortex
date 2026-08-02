package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// plannerStatsAnalysisLimit bounds how many entries ANALYZE samples per
// named graph index. The graph store only needs coarse relative cardinalities:
// without any sqlite_stat1 rows the planner costs alternative indexes by
// IN-probe count alone, and on a 480k-node workspace that served the hottest
// file projection from nodes_by_kind instead of the selective file index.
const plannerStatsAnalysisLimit = 1000

// plannerStatsIndexQuery is the synchronous, query-plan-critical subset of the
// graph indexes. Approximate ANALYZE still counts B-tree pages, so sampling all
// 17 named indexes traversed about 1.64 GiB and consumed 35 seconds on a cold
// production store. Most graph indexes either have no competing left-prefix
// access path or are explicitly selected with INDEXED BY; statistics do not
// change those plans. The exceptions below participate in real planner choices:
// node lookup/join order, edge-kind join order, and exact-site lookups where
// edges_by_from_line must beat the from_id-leading table key.
//
// Keep this list paired with the EXPLAIN plan locks in
// planner_stats_checkpoint_test.go. A new index belongs here only when a plan
// test demonstrates that its statistics change a production query choice.
const plannerStatsIndexQuery = `
WITH critical(name) AS (VALUES
  ('edges_by_from_line'),
  ('edges_by_kind'),
  ('nodes_by_file'),
  ('nodes_by_kind'),
  ('nodes_by_name'),
  ('nodes_by_repo'),
  ('nodes_by_repo_kind'),
  ('nodes_by_repo_language_name')
)
SELECT schema_index.name
FROM sqlite_schema AS schema_index
JOIN critical ON critical.name = schema_index.name
WHERE schema_index.type = 'index'
  AND schema_index.sql IS NOT NULL
ORDER BY schema_index.name`

// refreshPlannerStatsLocked recomputes sqlite_stat1 for the named graph
// indexes on the active write connection. Callers hold writeMu. PRAGMA optimize
// is intentionally not used here: it is driven by per-connection query history,
// while the cold pinned connection served writes only; forcing its 0x10000 bit
// would consider every FTS and sidecar table in the database.
func (s *Store) refreshPlannerStatsLocked(ctx context.Context) error {
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return err
	}
	defer release()
	return refreshPlannerStatsOnConn(ctx, conn)
}

func refreshPlannerStatsOnConn(ctx context.Context, conn *sql.Conn) error {
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA analysis_limit=%d`, plannerStatsAnalysisLimit)); err != nil {
		return err
	}

	// Materialize the tiny schema result and close its cursor before issuing
	// ANALYZE on the same physical connection. Re-entering a pinned connection
	// with an open rows cursor can deadlock database/sql.
	rows, err := conn.QueryContext(ctx, plannerStatsIndexQuery)
	if err != nil {
		return err
	}
	var indexes []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return err
		}
		indexes = append(indexes, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for _, name := range indexes {
		if _, err := conn.ExecContext(ctx, `ANALYZE `+quoteSQLiteIdentifier(name)); err != nil {
			return fmt.Errorf("analyze graph index %s: %w", name, err)
		}
	}
	return nil
}

func quoteSQLiteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// healPlannerStats backfills sqlite_stat1 for populated stores opened without
// graph-index statistics. Cold loads refresh stats at coordinated-bulk finalize;
// a warm restart of a store written before the engine kept planner statistics
// would otherwise plan blind for the rest of its life. Never fatal: a store
// without stats still answers every query, just through the default cost model.
func healPlannerStats(db *sql.DB) {
	var hasTable bool
	// sqlite_stat1 does not exist until the first ANALYZE, so probe the catalog
	// before the table. A sidecar-only stat row is not enough: nodes_by_file is
	// the sentinel for the graph-index refresh that protects the hottest plan.
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_stat1')`).Scan(&hasTable); err != nil {
		return
	}
	if hasTable {
		var populated bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_stat1 WHERE idx = 'nodes_by_file')`).Scan(&populated); err == nil && populated {
			return
		}
	}
	var hasNodes bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM nodes)`).Scan(&hasNodes); err != nil || !hasNodes {
		return
	}
	started := time.Now()
	ctx := context.Background()
	conn, err := db.Conn(ctx)
	if err != nil {
		return
	}
	defer conn.Close()
	if err := refreshPlannerStatsOnConn(ctx, conn); err != nil {
		log.Printf("store_sqlite: planner stats heal failed error=%q", err)
		return
	}
	log.Printf("store_sqlite: planner stats heal elapsed=%s", time.Since(started))
}
