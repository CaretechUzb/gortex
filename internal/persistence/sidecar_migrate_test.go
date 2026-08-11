package persistence

import (
	"bytes"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// openRaw opens a bare *sql.DB on path (driver registered by the package's
// modernc.org/sqlite blank import) for building fixtures and reading state
// without going through OpenSidecar's cache.
func openRaw(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw %s: %v", path, err)
	}
	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	return v
}

func hasColumn(t *testing.T, db *sql.DB, table, col string) bool {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid         int
			name, ctype string
			notnull, pk int
			dflt        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		if name == col {
			return true
		}
	}
	// A truncated table_info scan reads as "column absent", which would let
	// a migration assertion pass without the migration having run.
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info(%s): %v", table, err)
	}
	return false
}

func hasObject(t *testing.T, db *sql.DB, objType, name string) bool {
	t.Helper()
	var got string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type=? AND name=?", objType, name).Scan(&got)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatalf("lookup %s %s: %v", objType, name, err)
	}
	return got == name
}

// makeShapeC writes a pre-v0.46 sidecar: savings_events with its ORIGINAL
// 8 columns (no model, no client) and only the original indexes, with one
// row. user_version stays 0 — the state every install created before the
// migration framework reports. Opening this DB with the buggy code aborted
// with "no such column: model".
func makeShapeC(t *testing.T, path string) {
	t.Helper()
	db := openRaw(t, path)
	defer db.Close()
	_, err := db.Exec(`
CREATE TABLE savings_events (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	ts          INTEGER NOT NULL DEFAULT 0,
	session_id  TEXT NOT NULL DEFAULT '',
	tool        TEXT NOT NULL DEFAULT '',
	repo        TEXT NOT NULL DEFAULT '',
	language    TEXT NOT NULL DEFAULT '',
	returned    INTEGER NOT NULL DEFAULT 0,
	saved       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX idx_savings_events_ts   ON savings_events (ts);
CREATE INDEX idx_savings_events_tool ON savings_events (tool, ts);
CREATE TABLE savings_totals (bucket TEXT NOT NULL PRIMARY KEY, saved INTEGER NOT NULL DEFAULT 0, returned INTEGER NOT NULL DEFAULT 0, calls INTEGER NOT NULL DEFAULT 0) WITHOUT ROWID;
CREATE TABLE savings_meta (key TEXT NOT NULL PRIMARY KEY, value INTEGER NOT NULL DEFAULT 0) WITHOUT ROWID;
INSERT INTO savings_events (ts, tool, returned, saved) VALUES (123, 'get_symbol_source', 1000, 720);
`)
	if err != nil {
		t.Fatalf("build shape-C fixture: %v", err)
	}
}

// TestOpenSidecarFreshDB: a brand-new DB gets the full current shape and is
// stamped to the current version. The v1 migration's ALTERs hit the
// duplicate-column path (columns already present from CREATE TABLE) and must
// be swallowed inside the transaction without wedging the open.
func TestOpenSidecarFreshDB(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	st, err := OpenSidecar(path)
	if err != nil {
		t.Fatalf("OpenSidecar fresh: %v", err)
	}
	defer st.Close()

	if !hasColumn(t, st.db, "savings_events", "model") || !hasColumn(t, st.db, "savings_events", "client") {
		t.Fatal("fresh DB missing model/client columns")
	}
	if !hasObject(t, st.db, "index", "idx_savings_events_model") || !hasObject(t, st.db, "index", "idx_savings_events_client") {
		t.Fatal("fresh DB missing model/client indexes")
	}
	if v := userVersion(t, st.db); v != currentSidecarVersion {
		t.Fatalf("user_version = %d, want %d", v, currentSidecarVersion)
	}
}

// TestOpenSidecarMigratesShapeC reproduces the user-reported crash and proves
// the migration fixes it in place: the old 8-column DB opens cleanly, gains
// model/client + their indexes, is stamped to v1, and its existing row
// survives.
func TestOpenSidecarMigratesShapeC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	makeShapeC(t, path)

	st, err := OpenSidecar(path)
	if err != nil {
		t.Fatalf("OpenSidecar shape-C (the reported crash): %v", err)
	}
	defer st.Close()

	for _, col := range []string{"model", "client"} {
		if !hasColumn(t, st.db, "savings_events", col) {
			t.Fatalf("column %q not added by migration", col)
		}
	}
	for _, idx := range []string{"idx_savings_events_model", "idx_savings_events_client"} {
		if !hasObject(t, st.db, "index", idx) {
			t.Fatalf("index %q not created by migration", idx)
		}
	}
	if v := userVersion(t, st.db); v != currentSidecarVersion {
		t.Fatalf("user_version = %d, want %d", v, currentSidecarVersion)
	}

	var n int
	if err := st.db.QueryRow("SELECT COUNT(*) FROM savings_events").Scan(&n); err != nil {
		t.Fatalf("count savings_events: %v", err)
	}
	if n != 1 {
		t.Fatalf("savings row count = %d after migration, want 1 (data must survive)", n)
	}
}

// TestOpenSidecarShapeBCreatesMissingTables locks the load-bearing invariant
// that sidecarSchema runs on EVERY open (never gated on user_version): an old
// DB missing newer tables must gain them, and its data must survive.
func TestOpenSidecarShapeBCreatesMissingTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	func() {
		db := openRaw(t, path)
		defer db.Close()
		// A v0.36-era DB: it has scopes (an early table with no secondary
		// indexes, so it survives the base-schema rerun untouched) but not
		// the later savings_* / suppressions tables.
		if _, err := db.Exec(`CREATE TABLE scopes (name TEXT NOT NULL PRIMARY KEY, description TEXT NOT NULL DEFAULT '', repos TEXT NOT NULL DEFAULT '[]', paths TEXT NOT NULL DEFAULT '[]') WITHOUT ROWID;
INSERT INTO scopes (name, description) VALUES ('old-scope', 'keep me');`); err != nil {
			t.Fatalf("build shape-B fixture: %v", err)
		}
	}()

	st, err := OpenSidecar(path)
	if err != nil {
		t.Fatalf("OpenSidecar shape-B: %v", err)
	}
	defer st.Close()

	for _, tbl := range []string{"savings_events", "suppressions", "memories", "notebooks"} {
		if !hasObject(t, st.db, "table", tbl) {
			t.Fatalf("table %q not created on open of an old DB", tbl)
		}
	}
	if v := userVersion(t, st.db); v != currentSidecarVersion {
		t.Fatalf("user_version = %d, want %d", v, currentSidecarVersion)
	}
	var desc string
	if err := st.db.QueryRow("SELECT description FROM scopes WHERE name='old-scope'").Scan(&desc); err != nil {
		t.Fatalf("pre-existing scope lost: %v", err)
	}
	if desc != "keep me" {
		t.Fatalf("scope description = %q, want 'keep me'", desc)
	}
}

// TestOpenSidecarIdempotentReopen: a second open of a migrated DB is a clean
// no-op that leaves the version unchanged.
func TestOpenSidecarIdempotentReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	makeShapeC(t, path)

	st, err := OpenSidecar(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := st.Close(); err != nil { // drops the cache entry so the reopen re-runs the path
		t.Fatalf("close: %v", err)
	}

	st2, err := OpenSidecar(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer st2.Close()
	if v := userVersion(t, st2.db); v != currentSidecarVersion {
		t.Fatalf("user_version after reopen = %d, want %d", v, currentSidecarVersion)
	}
}

// TestApplyOneFailureLeavesVersionUnchanged: a failing migration rolls back
// and does not advance user_version, so the next open retries it.
func TestApplyOneFailureLeavesVersionUnchanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sqlite")
	db := openRaw(t, path)
	defer db.Close()

	boom := migration{version: 1, name: "boom", fn: func(tx *sql.Tx) error {
		if _, err := tx.Exec("CREATE TABLE partial (x TEXT)"); err != nil {
			return err
		}
		return fmt.Errorf("synthetic failure after a partial write")
	}}
	if err := applyOne(db, boom); err == nil {
		t.Fatal("expected applyOne to surface the migration error")
	}
	if v := userVersion(t, db); v != 0 {
		t.Fatalf("user_version = %d after failed migration, want 0", v)
	}
	if hasObject(t, db, "table", "partial") {
		t.Fatal("partial table from the failed migration was not rolled back")
	}
}

// TestAddColumnIfMissingTxScope: only "duplicate column name" is swallowed;
// every other ALTER error propagates (so a real failure is not masked).
func TestAddColumnIfMissingTxScope(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.sqlite")
	db := openRaw(t, path)
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (a TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := addColumnIfMissingTx(tx, "t", "b", "TEXT NOT NULL DEFAULT ''"); err != nil {
		t.Fatalf("adding a new column should succeed: %v", err)
	}
	if err := addColumnIfMissingTx(tx, "t", "b", "TEXT NOT NULL DEFAULT ''"); err != nil {
		t.Fatalf("duplicate column should be swallowed: %v", err)
	}
	if err := addColumnIfMissingTx(tx, "does_not_exist", "x", "TEXT"); err == nil {
		t.Fatal("a non-duplicate error (no such table) must propagate")
	}
}

// TestOpenSidecarConcurrentProcesses is the regression guard for the headline
// concurrency hazard: several processes opening a stale DB at once must ALL
// succeed (one migrates under the IMMEDIATE write lock, the others wait on
// busy_timeout and skip). With a plain DEFERRED begin the losers hit an
// un-retryable SQLITE_BUSY — this test fails without _txlock=immediate.
func TestOpenSidecarConcurrentProcesses(t *testing.T) {
	if path := os.Getenv("GORTEX_SIDECAR_MIGRATE_CHILD"); path != "" {
		// Child process: open once, report via exit code, never spawn.
		st, err := OpenSidecar(path)
		if err != nil {
			fmt.Fprintln(os.Stderr, "child OpenSidecar failed:", err)
			os.Exit(3)
		}
		_ = st.Close()
		os.Exit(0)
	}

	path := filepath.Join(t.TempDir(), "sidecar.sqlite")
	makeShapeC(t, path) // stale DB whose first open must migrate

	const procs = 6
	cmds := make([]*exec.Cmd, procs)
	outs := make([]*bytes.Buffer, procs)
	for i := range cmds {
		buf := &bytes.Buffer{}
		cmd := exec.Command(os.Args[0], "-test.run=^TestOpenSidecarConcurrentProcesses$", "-test.count=1")
		cmd.Env = append(os.Environ(), "GORTEX_SIDECAR_MIGRATE_CHILD="+path)
		cmd.Stderr = buf
		cmds[i], outs[i] = cmd, buf
	}
	for i, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			t.Fatalf("start child %d: %v", i, err)
		}
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("concurrent opener %d errored (none may): %v\nstderr: %s", i, err, outs[i].String())
		}
	}

	db := openRaw(t, path)
	defer db.Close()
	if v := userVersion(t, db); v != currentSidecarVersion {
		t.Fatalf("user_version = %d after concurrent opens, want %d", v, currentSidecarVersion)
	}
	if !hasColumn(t, db, "savings_events", "model") || !hasColumn(t, db, "savings_events", "client") {
		t.Fatal("model/client columns missing after concurrent migration")
	}
}
