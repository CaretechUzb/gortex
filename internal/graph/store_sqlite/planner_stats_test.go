package store_sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func plannerStatRows(t *testing.T, s *Store) int {
	t.Helper()
	var hasTable bool
	if err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'sqlite_stat1')`).Scan(&hasTable); err != nil {
		t.Fatalf("probe sqlite_stat1: %v", err)
	}
	if !hasTable {
		return 0
	}
	var rows int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_stat1`).Scan(&rows); err != nil {
		t.Fatalf("count sqlite_stat1: %v", err)
	}
	return rows
}

func seedPlannerStatsNodes(s *Store) {
	var nodes []*graph.Node
	for i := 0; i < 64; i++ {
		nodes = append(nodes, &graph.Node{
			ID:       fmt.Sprintf("pkg/a.go::sym%02d", i),
			Name:     fmt.Sprintf("sym%02d", i),
			Kind:     graph.KindFunction,
			FilePath: "pkg/a.go",
			Language: "go",
		})
	}
	s.AddBatch(nodes, nil)
}

// A coordinated cold load must leave planner statistics behind: every
// post-load phase plans against the store, and a stats-blind planner picks
// indexes by IN-probe count alone (the round-7 whale).
func TestCoordinatedBulkFinalizeRefreshesPlannerStats(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "stats_bulk.sqlite"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if !s.BeginCoordinatedBulkLoad() {
		t.Fatal("BeginCoordinatedBulkLoad refused")
	}
	seedPlannerStatsNodes(s)
	s.AddEdge(&graph.Edge{
		From:     "pkg/a.go::sym00",
		To:       "pkg/a.go::sym01",
		Kind:     graph.EdgeCalls,
		FilePath: "pkg/a.go",
	})
	if err := s.EndCoordinatedBulkLoad(); err != nil {
		t.Fatalf("EndCoordinatedBulkLoad: %v", err)
	}
	if rows := plannerStatRows(t, s); rows == 0 {
		t.Fatal("bulk finalize left no sqlite_stat1 rows")
	}
	statsRows, err := s.db.Query(`SELECT DISTINCT tbl FROM sqlite_stat1 ORDER BY tbl`)
	if err != nil {
		t.Fatalf("list analyzed tables: %v", err)
	}
	defer statsRows.Close()
	var tables []string
	for statsRows.Next() {
		var table string
		if err := statsRows.Scan(&table); err != nil {
			t.Fatalf("scan analyzed table: %v", err)
		}
		tables = append(tables, table)
	}
	if err := statsRows.Err(); err != nil {
		t.Fatalf("iterate analyzed tables: %v", err)
	}
	if got, want := fmt.Sprint(tables), "[edges nodes]"; got != want {
		t.Fatalf("analyzed tables = %s, want %s", got, want)
	}
}

// A populated store written before the engine kept planner statistics must
// be healed at Open — a warm restart never passes through bulk finalize.
func TestOpenHealsPlannerStats(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats_heal.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	seedPlannerStatsNodes(s)
	// Erase any statistics so the reopen sees the pre-heal state.
	if _, err := s.writerDB.Exec(`DELETE FROM sqlite_stat1`); err != nil {
		// The table only exists once ANALYZE has run; absence is the state
		// under test, not a failure.
		t.Logf("clear sqlite_stat1: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if rows := plannerStatRows(t, reopened); rows == 0 {
		t.Fatal("open did not heal sqlite_stat1 for a populated store")
	}
}

func TestOpenHealsMissingBoundedSitePlannerStat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats_site_heal.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	seedPlannerStatsNodes(s)
	s.AddEdge(&graph.Edge{
		From:     "pkg/a.go::sym00",
		To:       "pkg/a.go::sym01",
		Kind:     graph.EdgeCalls,
		FilePath: "pkg/a.go",
		Line:     7,
	})
	s.writeMu.Lock()
	err = s.refreshPlannerStatsLocked(context.Background())
	s.writeMu.Unlock()
	if err != nil {
		t.Fatalf("seed graph planner stats: %v", err)
	}
	if _, err := s.writerDB.Exec(`DROP INDEX edges_by_from_line_kind`); err != nil {
		t.Fatalf("drop bounded-site index: %v", err)
	}
	var warmState bool
	if err := s.db.QueryRow(`
SELECT EXISTS(SELECT 1 FROM sqlite_stat1 WHERE idx = 'nodes_by_file')
   AND NOT EXISTS(SELECT 1 FROM sqlite_stat1 WHERE idx = 'edges_by_from_line_kind')`).Scan(&warmState); err != nil {
		t.Fatalf("probe pre-upgrade planner stats: %v", err)
	}
	if !warmState {
		t.Fatal("fixture did not retain old graph stats while removing the new sibling stat")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close pre-upgrade store: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	var healed bool
	if err := reopened.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_stat1 WHERE idx = 'edges_by_from_line_kind')`).Scan(&healed); err != nil {
		t.Fatalf("probe healed bounded-site stat: %v", err)
	}
	if !healed {
		t.Fatal("open recreated the bounded-site index without healing its planner stat")
	}
}
