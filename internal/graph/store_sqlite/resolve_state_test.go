package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

const (
	resolveStateAbruptModeEnv = "GORTEX_RESOLVE_STATE_ABRUPT_MODE"
	resolveStateAbruptPathEnv = "GORTEX_RESOLVE_STATE_ABRUPT_PATH"

	resolveStateAbruptBeginMode    = "begin"
	resolveStateAbruptCompleteMode = "complete"
)

func TestResolveStateDurabilityAcrossAbruptProcessExit(t *testing.T) {
	if mode := os.Getenv(resolveStateAbruptModeEnv); mode != "" {
		if err := runResolveStateAbruptChild(mode, os.Getenv(resolveStateAbruptPathEnv)); err != nil {
			fmt.Fprintf(os.Stderr, "resolve-state abrupt child: %v\n", err)
			os.Exit(2)
		}
		// Deliberately bypass Store.Close, the bulk-load teardown, and SQLite
		// checkpoints. The parent must observe only state made durable by the
		// FULL-synchronous resolve-state transactions themselves.
		os.Exit(0)
	}

	t.Run("interrupted marker flushes preceding graph frame", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "graph.db")
		prepareResolveStateAbruptDB(t, path)
		runResolveStateAbruptSubprocess(t, resolveStateAbruptBeginMode, path)

		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if node := store.GetNode("repo/before-begin.go"); node == nil {
			t.Fatal("graph frame committed before durable begin did not survive abrupt exit")
		}

		observed, incomplete, err := store.ResolvePassIncomplete()
		if err != nil {
			t.Fatal(err)
		}
		if !incomplete || observed.Generation <= 0 || observed.Owned {
			t.Fatalf("reopened state = (%+v, %v), want positive interrupted generation", observed, incomplete)
		}
		joined, err := store.BeginResolvePass()
		if err != nil {
			t.Fatal(err)
		}
		if joined.Owned || joined.Generation != observed.Generation {
			t.Fatalf("begin after abrupt exit = %+v, want join generation %d", joined, observed.Generation)
		}

		wrong := graph.ResolveStateToken{Generation: observed.Generation + 1}
		if err := store.CompleteResolvePass(wrong); err == nil {
			t.Fatal("stale generation unexpectedly cleared abruptly persisted resolve state")
		}
		stillActive, incomplete, err := store.ResolvePassIncomplete()
		if err != nil {
			t.Fatal(err)
		}
		if !incomplete || stillActive.Generation != observed.Generation {
			t.Fatalf("CAS failure changed abrupt state to (%+v, %v)", stillActive, incomplete)
		}
		if err := store.CompleteResolvePass(observed); err != nil {
			t.Fatal(err)
		}
		if _, incomplete, err := store.ResolvePassIncomplete(); err != nil || incomplete {
			t.Fatalf("completed abrupt state = (incomplete=%v, err=%v), want clean", incomplete, err)
		}
	})

	t.Run("completed marker flushes preceding graph frame", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "graph.db")
		prepareResolveStateAbruptDB(t, path)
		runResolveStateAbruptSubprocess(t, resolveStateAbruptCompleteMode, path)

		store, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		if node := store.GetNode("repo/before-complete.go"); node == nil {
			t.Fatal("bulk graph frame committed before durable completion did not survive abrupt exit")
		}
		if _, incomplete, err := store.ResolvePassIncomplete(); err != nil || incomplete {
			t.Fatalf("reopened completed state = (incomplete=%v, err=%v), want clean", incomplete, err)
		}

		owner, err := store.BeginResolvePass()
		if err != nil {
			t.Fatal(err)
		}
		if !owner.Owned || owner.Generation <= 0 {
			t.Fatalf("begin after durable abrupt completion = %+v, want new owner", owner)
		}
		if err := store.CompleteResolvePass(owner); err != nil {
			t.Fatal(err)
		}
	})
}

func prepareResolveStateAbruptDB(t *testing.T, path string) {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func runResolveStateAbruptSubprocess(t *testing.T, mode, path string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestResolveStateDurabilityAcrossAbruptProcessExit$")
	cmd.Env = append(os.Environ(),
		resolveStateAbruptModeEnv+"="+mode,
		resolveStateAbruptPathEnv+"="+path,
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("abrupt child %q failed: %v\n%s", mode, err, output)
	}
}

func runResolveStateAbruptChild(mode, path string) error {
	if path == "" {
		return errors.New("missing abrupt-child database path")
	}
	store, err := Open(path)
	if err != nil {
		return err
	}
	if !store.BeginCoordinatedBulkLoad() {
		return errors.New("abrupt child did not enter coordinated bulk mode")
	}
	if got, err := resolveStateSynchronousMode(store.bulkConn); err != nil {
		return err
	} else if got != 0 {
		return fmt.Errorf("initial bulk synchronous mode = %d, want OFF", got)
	}

	switch mode {
	case resolveStateAbruptBeginMode:
		store.AddNode(&graph.Node{
			ID:       "repo/before-begin.go",
			Kind:     graph.KindFile,
			Name:     "before-begin.go",
			FilePath: "repo/before-begin.go",
		})
		token, err := store.BeginResolvePass()
		if err != nil {
			return err
		}
		if !token.Owned || token.Generation <= 0 {
			return fmt.Errorf("begin token = %+v, want owned positive generation", token)
		}
	case resolveStateAbruptCompleteMode:
		token, err := store.BeginResolvePass()
		if err != nil {
			return err
		}
		if !token.Owned || token.Generation <= 0 {
			return fmt.Errorf("begin token = %+v, want owned positive generation", token)
		}
		store.AddNode(&graph.Node{
			ID:       "repo/before-complete.go",
			Kind:     graph.KindFile,
			Name:     "before-complete.go",
			FilePath: "repo/before-complete.go",
		})
		if err := store.CompleteResolvePass(token); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown abrupt-child mode %q", mode)
	}

	if got, err := resolveStateSynchronousMode(store.bulkConn); err != nil {
		return err
	} else if got != 0 {
		return fmt.Errorf("bulk synchronous mode after %s = %d, want restored OFF", mode, got)
	}
	return nil
}

func resolveStateSynchronousMode(conn *sql.Conn) (int, error) {
	if conn == nil {
		return 0, errors.New("expected pinned bulk connection")
	}
	var mode int
	err := conn.QueryRowContext(context.Background(), `PRAGMA synchronous`).Scan(&mode)
	return mode, err
}

func synchronousMode(t *testing.T, conn *sql.Conn) int {
	t.Helper()
	mode, err := resolveStateSynchronousMode(conn)
	if err != nil {
		t.Fatal(err)
	}
	return mode
}
