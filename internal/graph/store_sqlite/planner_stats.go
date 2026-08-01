package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"
)

// plannerStatsAnalysisLimit bounds how many index entries ANALYZE samples
// per index. The graph store only needs coarse relative cardinalities:
// without any sqlite_stat1 rows the planner costs alternative indexes by
// IN-probe count alone, and on a 480k-node workspace that served the
// hottest file projection from nodes_by_kind — scanning whole kind ranges
// with a main-table B-tree seek per entry instead of ~50-row file probes.
// The bounded sample keeps a refresh cheap even on multi-gigabyte stores.
const plannerStatsAnalysisLimit = 1000

// refreshPlannerStatsLocked recomputes sqlite_stat1 on the active write
// connection. Callers hold writeMu; inside a bulk-load window the statements
// run on the pinned bulk connection, otherwise on the canonical writer. Both
// are single physical connections, so the analysis_limit pragma governs the
// ANALYZE that follows it.
func (s *Store) refreshPlannerStatsLocked(ctx context.Context) error {
	if _, err := s.execActiveWriteLocked(ctx, fmt.Sprintf(`PRAGMA analysis_limit=%d`, plannerStatsAnalysisLimit)); err != nil {
		return err
	}
	if _, err := s.execActiveWriteLocked(ctx, `ANALYZE nodes`); err != nil {
		return err
	}
	_, err := s.execActiveWriteLocked(ctx, `ANALYZE edges`)
	return err
}

// healPlannerStats backfills sqlite_stat1 for populated stores opened
// without it. Cold loads refresh stats at coordinated-bulk finalize; a warm
// restart of a store written before the engine kept planner statistics
// would otherwise plan blind for the rest of its life. Never fatal: a store
// without stats still answers every query, just through the planner's
// default cost model.
func healPlannerStats(db *sql.DB) {
	var hasTable bool
	// sqlite_stat1 does not exist until the first ANALYZE, so probe the
	// catalog before the table.
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_stat1')`).Scan(&hasTable); err != nil {
		return
	}
	if hasTable {
		var populated bool
		if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_stat1)`).Scan(&populated); err == nil && populated {
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
	if _, err := conn.ExecContext(ctx, fmt.Sprintf(`PRAGMA analysis_limit=%d`, plannerStatsAnalysisLimit)); err != nil {
		log.Printf("store_sqlite: planner stats heal failed error=%q", err)
		return
	}
	if _, err := conn.ExecContext(ctx, `ANALYZE nodes`); err != nil {
		log.Printf("store_sqlite: planner stats heal failed error=%q", err)
		return
	}
	if _, err := conn.ExecContext(ctx, `ANALYZE edges`); err != nil {
		log.Printf("store_sqlite: planner stats heal failed error=%q", err)
		return
	}
	log.Printf("store_sqlite: planner stats heal elapsed=%s", time.Since(started))
}
