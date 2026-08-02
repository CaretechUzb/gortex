package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

func TestRefreshPlannerStatsTargetsNamedGraphIndexes(t *testing.T) {
	store := openPlannerStatsTestStore(t)
	_, err := store.writerDB.Exec(`
WITH digits(d) AS (VALUES(0),(1),(2),(3),(4),(5),(6),(7),(8),(9)),
seq(x) AS (
    SELECT a.d*1000 + b.d*100 + c.d*10 + d.d + 1
    FROM digits AS a, digits AS b, digits AS c, digits AS d
    LIMIT 5000
)
INSERT INTO nodes(id, kind, name, file_path)
SELECT printf('node-%05d', x), 'function', printf('fn%d', x),
       CASE WHEN x = 1 THEN 'target.go' ELSE printf('file-%05d.go', x) END
FROM seq`)
	if err != nil {
		t.Fatalf("populate planner fixture: %v", err)
	}
	_, err = store.writerDB.Exec(`
WITH digits(d) AS (VALUES(0),(1),(2),(3),(4),(5),(6),(7),(8),(9)),
seq(x) AS (
    SELECT a.d*1000 + b.d*100 + c.d*10 + d.d + 1
    FROM digits AS a, digits AS b, digits AS c, digits AS d
    LIMIT 5000
)
INSERT INTO edges(from_id, to_id, kind, file_path, line)
SELECT 'hub', printf('node-%05d', x), 'calls', 'hub.go', x
FROM seq`)
	if err != nil {
		t.Fatalf("populate edge planner fixture: %v", err)
	}

	store.writeMu.Lock()
	err = store.refreshPlannerStatsLocked(t.Context())
	store.writeMu.Unlock()
	if err != nil {
		t.Fatalf("refresh planner stats: %v", err)
	}

	rows, err := store.db.Query(`SELECT idx FROM sqlite_stat1 WHERE tbl IN ('nodes', 'edges') ORDER BY idx`)
	if err != nil {
		t.Fatalf("query graph planner stats: %v", err)
	}
	var gotIndexes []string
	for rows.Next() {
		var index string
		if err := rows.Scan(&index); err != nil {
			_ = rows.Close()
			t.Fatalf("scan graph planner stat: %v", err)
		}
		gotIndexes = append(gotIndexes, index)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate graph planner stats: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close graph planner stats: %v", err)
	}
	wantIndexes := []string{
		"edges_by_from_line",
		"edges_by_kind",
		"nodes_by_file",
		"nodes_by_kind",
		"nodes_by_name",
		"nodes_by_repo",
		"nodes_by_repo_kind",
		"nodes_by_repo_language_name",
	}
	if !reflect.DeepEqual(gotIndexes, wantIndexes) {
		t.Fatalf("synchronous graph stats = %v, want %v", gotIndexes, wantIndexes)
	}

	plan := explainPlannerQueryPlan(t, store.db,
		`SELECT id FROM nodes WHERE file_path = ? AND kind = ?`,
		"target.go", "function")
	if !strings.Contains(plan, "nodes_by_file") {
		t.Fatalf("selective file query missed nodes_by_file after stats refresh:\n%s", plan)
	}

	// The table's UNIQUE key also starts with from_id. On the production
	// corpus a stats-blind planner chose that key and reread every edge owned by
	// a hub source; the line-bearing stat must keep exact-site probes selective.
	plan = explainPlannerQueryPlan(t, store.db,
		`SELECT to_id FROM edges WHERE from_id = ? AND line = ? AND kind = ?`,
		"hub", 1, "calls")
	if !strings.Contains(plan, "edges_by_from_line") {
		t.Fatalf("exact-site query missed edges_by_from_line after stats refresh:\n%s", plan)
	}

	// These indexes have no competing left-prefix path. They remain selected
	// without paying synchronous ANALYZE page counts for them.
	plan = explainPlannerQueryPlan(t, store.db,
		`SELECT from_id FROM edges WHERE to_id = ? AND kind = ?`,
		"node-00001", "calls")
	if !strings.Contains(plan, "edges_by_to") {
		t.Fatalf("in-edge query missed edges_by_to without a dedicated stat:\n%s", plan)
	}
	plan = explainPlannerQueryPlan(t, store.db,
		`SELECT from_id FROM edges WHERE file_path = ? AND kind = ?`,
		"hub.go", "calls")
	if !strings.Contains(plan, "edges_by_file") {
		t.Fatalf("file-edge query missed edges_by_file without a dedicated stat:\n%s", plan)
	}
}

func TestEndCoordinatedBulkLoadDoesNotWaitForReadSnapshot(t *testing.T) {
	store := openPlannerStatsTestStore(t)
	store.passiveCheckpointTimeout = 250 * time.Millisecond
	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("cold store did not enter coordinated bulk load")
	}
	store.AddNode(&graph.Node{ID: "before", Kind: graph.KindFunction, Name: "before", FilePath: "before.go"})

	// Hold an explicit old read snapshot across another committed write. Such a
	// snapshot prevents RESTART/TRUNCATE from resetting the WAL even though the
	// Store writer gate is exclusively held by finalization.
	readTx, err := store.db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read snapshot: %v", err)
	}
	defer func() { _ = readTx.Rollback() }()
	var count int
	if err := readTx.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&count); err != nil {
		t.Fatalf("establish read snapshot: %v", err)
	}
	store.AddNode(&graph.Node{ID: "after", Kind: graph.KindFunction, Name: "after", FilePath: "after.go"})

	var events []bulkFinalizeEvent
	var checkpoint bulkFinalizeEvent
	store.bulkFinalizeObserver = func(event bulkFinalizeEvent) {
		events = append(events, event)
		if event.Stage == "checkpoint" {
			checkpoint = event
		}
	}
	started := time.Now()
	if err := store.EndCoordinatedBulkLoad(); err != nil {
		t.Fatalf("end coordinated bulk load: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("reader snapshot delayed cold finalization by %s", elapsed)
	}
	preStats, plannerStats := -1, -1
	for i, event := range events {
		switch {
		case event.Stage == "checkpoint_passive" && event.Name == "planner_stats_before":
			preStats = i
		case event.Stage == "planner_stats":
			plannerStats = i
		}
	}
	if preStats < 0 || plannerStats < 0 || preStats >= plannerStats {
		t.Fatalf("bulk finalization event order missing pre-stats checkpoint: %+v", events)
	}
	if checkpoint.Name != "wal_passive" {
		t.Fatalf("final checkpoint = %q, want wal_passive", checkpoint.Name)
	}
	if checkpoint.Err != nil {
		t.Fatalf("passive checkpoint failed: %v", checkpoint.Err)
	}
}

func explainPlannerQueryPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.QueryContext(t.Context(), `EXPLAIN QUERY PLAN `+query, args...)
	if err != nil {
		t.Fatalf("explain query plan: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Errorf("close query plan: %v", err)
		}
	}()

	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate query plan: %v", err)
	}
	return strings.Join(details, "\n")
}

func openPlannerStatsTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Errorf("close sqlite store: %v", err)
		}
	})
	return store
}
