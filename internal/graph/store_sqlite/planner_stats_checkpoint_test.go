package store_sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
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

	store.writeMu.Lock()
	err = store.refreshPlannerStatsLocked(t.Context())
	store.writeMu.Unlock()
	if err != nil {
		t.Fatalf("refresh planner stats: %v", err)
	}

	for _, index := range []string{"nodes_by_file", "nodes_by_kind", "nodes_by_name", "nodes_by_repo_kind"} {
		var present bool
		if err := store.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_stat1 WHERE idx = ?)`, index).Scan(&present); err != nil {
			t.Fatalf("query stat for %s: %v", index, err)
		}
		if !present {
			t.Fatalf("named graph index %s has no planner stat", index)
		}
	}

	rows, err := store.db.QueryContext(t.Context(),
		`EXPLAIN QUERY PLAN SELECT id FROM nodes WHERE file_path = ? AND kind = ?`,
		"target.go", "function")
	if err != nil {
		t.Fatalf("explain selective file query: %v", err)
	}
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			_ = rows.Close()
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatalf("iterate query plan: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close query plan: %v", err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "nodes_by_file") {
		t.Fatalf("selective file query missed nodes_by_file after stats refresh:\n%s", plan)
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

	var checkpoint bulkFinalizeEvent
	store.bulkFinalizeObserver = func(event bulkFinalizeEvent) {
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
	if checkpoint.Name != "wal_passive" {
		t.Fatalf("final checkpoint = %q, want wal_passive", checkpoint.Name)
	}
	if checkpoint.Err != nil {
		t.Fatalf("passive checkpoint failed: %v", checkpoint.Err)
	}
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
