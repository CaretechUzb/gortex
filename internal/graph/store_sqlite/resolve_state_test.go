package store_sqlite

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestResolveStatePersistsOwnershipAndCASAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	owner, err := store.BeginResolvePass()
	if err != nil {
		t.Fatal(err)
	}
	if !owner.Owned || owner.Generation <= 0 {
		t.Fatalf("first begin = %+v, want owned positive generation", owner)
	}
	joined, err := store.BeginResolvePass()
	if err != nil {
		t.Fatal(err)
	}
	if joined.Owned || joined.Generation != owner.Generation {
		t.Fatalf("nested begin = %+v, want unowned generation %d", joined, owner.Generation)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	observed, incomplete, err := store.ResolvePassIncomplete()
	if err != nil {
		t.Fatal(err)
	}
	if !incomplete || observed.Generation != owner.Generation || observed.Owned {
		t.Fatalf("reopened state = (%+v, %v), want generation %d present and unowned", observed, incomplete, owner.Generation)
	}

	wrong := graph.ResolveStateToken{Generation: owner.Generation + 1}
	if err := store.CompleteResolvePass(wrong); err == nil {
		t.Fatal("stale generation unexpectedly cleared active resolve state")
	}
	observed, incomplete, err = store.ResolvePassIncomplete()
	if err != nil {
		t.Fatal(err)
	}
	if !incomplete || observed.Generation != owner.Generation {
		t.Fatalf("CAS failure changed state to (%+v, %v)", observed, incomplete)
	}
	if err := store.CompleteResolvePass(observed); err != nil {
		t.Fatal(err)
	}
	if _, incomplete, err := store.ResolvePassIncomplete(); err != nil || incomplete {
		t.Fatalf("completed state = (incomplete=%v, err=%v), want clean", incomplete, err)
	}
}

func TestResolveStateCommitPrecedesBulkResolverWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("empty on-disk store did not enter coordinated bulk mode")
	}
	defer func() {
		if err := store.EndCoordinatedBulkLoad(); err != nil {
			t.Errorf("end coordinated bulk load: %v", err)
		}
	}()
	if got := synchronousMode(t, store.bulkConn); got != 0 {
		t.Fatalf("bulk synchronous mode = %d, want OFF", got)
	}

	token, err := store.BeginResolvePass()
	if err != nil {
		t.Fatal(err)
	}
	if !token.Owned {
		t.Fatalf("begin token = %+v, want owner", token)
	}
	if got := synchronousMode(t, store.bulkConn); got != 0 {
		t.Fatalf("bulk synchronous mode after durable begin = %d, want restored OFF", got)
	}

	// Read through a separate connection while the bulk writer remains pinned.
	// Visibility here proves Begin committed before the caller can issue its
	// first independently durable resolver-page write.
	reader, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var generation int64
	if err := reader.QueryRow(`SELECT generation FROM resolve_state WHERE slot = 1`).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != token.Generation {
		t.Fatalf("separate connection saw generation %d, want %d", generation, token.Generation)
	}

	store.AddNode(&graph.Node{ID: "repo/a.go", Kind: graph.KindFile, Name: "a.go", FilePath: "repo/a.go"})
	if err := store.CompleteResolvePass(token); err != nil {
		t.Fatal(err)
	}
	if got := synchronousMode(t, store.bulkConn); got != 0 {
		t.Fatalf("bulk synchronous mode after durable complete = %d, want restored OFF", got)
	}
	var count int
	if err := reader.QueryRow(`SELECT COUNT(*) FROM resolve_state`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("resolve_state rows after completion = %d, want 0", count)
	}
}

func TestResolveStateBeginRollsBackAndRestoresBulkMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.writerDB.Exec(`
CREATE TRIGGER reject_resolve_state
BEFORE INSERT ON resolve_state
BEGIN
    SELECT RAISE(ABORT, 'forced resolve-state failure');
END`); err != nil {
		t.Fatal(err)
	}
	if !store.BeginCoordinatedBulkLoad() {
		t.Fatal("empty on-disk store did not enter coordinated bulk mode")
	}
	defer func() {
		if err := store.EndCoordinatedBulkLoad(); err != nil {
			t.Errorf("end coordinated bulk load: %v", err)
		}
	}()
	if _, err := store.BeginResolvePass(); err == nil {
		t.Fatal("forced marker insert unexpectedly succeeded")
	}
	if got := synchronousMode(t, store.bulkConn); got != 0 {
		t.Fatalf("bulk synchronous mode after rollback = %d, want restored OFF", got)
	}
	if _, incomplete, err := store.ResolvePassIncomplete(); err != nil || incomplete {
		t.Fatalf("rolled-back begin = (incomplete=%v, err=%v), want clean", incomplete, err)
	}
}

func TestResolveStateMigrationIsIdempotentAndAtomic(t *testing.T) {
	t.Run("idempotent", func(t *testing.T) {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "migration.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		steps := []schemaMigration{
			{version: 10, name: "resolve state first", inPlace: createResolveStateTable},
			{version: 11, name: "resolve state repeated", inPlace: createResolveStateTable},
		}
		if err := applyInPlaceMigrations(db, steps); err != nil {
			t.Fatal(err)
		}
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'resolve_state'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("resolve_state table count = %d, want 1", count)
		}
	})

	t.Run("atomic rollback", func(t *testing.T) {
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "rollback.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		forced := errors.New("forced migration failure")
		steps := []schemaMigration{
			{version: 10, name: "resolve state", inPlace: createResolveStateTable},
			{version: 11, name: "fail", inPlace: func(*sql.Tx) error { return forced }},
		}
		if err := applyInPlaceMigrations(db, steps); !errors.Is(err, forced) {
			t.Fatalf("migration error = %v, want %v", err, forced)
		}
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'resolve_state'`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("resolve_state survived rolled-back migration: count=%d", count)
		}
	})
}

func synchronousMode(t *testing.T, conn *sql.Conn) int {
	t.Helper()
	if conn == nil {
		t.Fatal("expected pinned bulk connection")
	}
	var mode int
	if err := conn.QueryRowContext(t.Context(), `PRAGMA synchronous`).Scan(&mode); err != nil {
		t.Fatal(err)
	}
	return mode
}
