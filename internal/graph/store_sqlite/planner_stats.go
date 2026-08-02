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

// plannerStatsIndexQuery deliberately excludes SQLite's automatic indexes and
// the nodes/edges table b-trees themselves. ANALYZE nodes/edges visits those
// broad surfaces as well as every secondary index and took 51 seconds on a
// production cold store even with analysis_limit set. The named indexes are
// the planner alternatives whose relative cardinalities matter to graph reads.
// Discovering them from sqlite_schema keeps a future named graph index covered
// without coupling this path to every DDL declaration site.
const plannerStatsIndexQuery = `
SELECT name
FROM sqlite_schema
WHERE type = 'index'
  AND tbl_name IN ('nodes', 'edges')
  AND sql IS NOT NULL
ORDER BY name`

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
