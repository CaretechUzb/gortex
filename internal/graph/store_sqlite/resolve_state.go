package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

const resolveStateSlot = 1

var _ graph.ResolveStateStore = (*Store)(nil)

// BeginResolvePass durably creates or joins the singleton resolution
// generation. The transaction commits with synchronous=FULL even when a cold
// bulk window has pinned the writer at synchronous=OFF, so the marker reaches
// stable storage before this method returns and before any resolver page can be
// committed separately.
func (s *Store) BeginResolvePass() (graph.ResolveStateToken, error) {
	var token graph.ResolveStateToken
	err := s.withDurableResolveStateTx(func(tx *sql.Tx) error {
		result, err := tx.Exec(`
INSERT OR IGNORE INTO resolve_state(slot, generation, started_at_unix)
VALUES (?, 1 + (random() & 4611686018427387903), ?)`, resolveStateSlot, time.Now().Unix())
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if err := tx.QueryRow(`SELECT generation FROM resolve_state WHERE slot = ?`, resolveStateSlot).
			Scan(&token.Generation); err != nil {
			return err
		}
		token.Owned = inserted == 1
		return nil
	})
	if err != nil {
		return graph.ResolveStateToken{}, fmt.Errorf("begin resolve generation: %w", err)
	}
	return token, nil
}

// ResolvePassIncomplete reads the active generation under the writer gate so a
// concurrent in-process Begin/Complete cannot race the warmup probe.
func (s *Store) ResolvePassIncomplete() (graph.ResolveStateToken, bool, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx := context.Background()
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return graph.ResolveStateToken{}, false, fmt.Errorf("open resolve-state reader: %w", err)
	}
	defer release()

	var generation int64
	err = conn.QueryRowContext(ctx,
		`SELECT generation FROM resolve_state WHERE slot = ?`, resolveStateSlot,
	).Scan(&generation)
	if errors.Is(err, sql.ErrNoRows) {
		return graph.ResolveStateToken{}, false, nil
	}
	if err != nil {
		return graph.ResolveStateToken{}, false, fmt.Errorf("read resolve generation: %w", err)
	}
	return graph.ResolveStateToken{Generation: generation}, true, nil
}

// CompleteResolvePass removes only the exact generation supplied by the
// successful outer pipeline. A stale owner can never clear a newer pass.
func (s *Store) CompleteResolvePass(token graph.ResolveStateToken) error {
	if token.Generation <= 0 {
		return fmt.Errorf("invalid resolve generation %d", token.Generation)
	}
	err := s.withDurableResolveStateTx(func(tx *sql.Tx) error {
		result, err := tx.Exec(
			`DELETE FROM resolve_state WHERE slot = ? AND generation = ?`,
			resolveStateSlot, token.Generation,
		)
		if err != nil {
			return err
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if removed != 1 {
			return fmt.Errorf("resolve generation %d is no longer active", token.Generation)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("complete resolve generation %d: %w", token.Generation, err)
	}
	return nil
}

// withDurableResolveStateTx serializes marker changes with graph writes and
// temporarily raises the active writer connection to FULL durability. The
// active connection may be the max-one writer pool connection or the same
// connection pinned by a coordinated cold bulk load; reusing it avoids writer
// pool deadlock and gives a strict commit-before-resolver-page ordering.
func (s *Store) withDurableResolveStateTx(apply func(*sql.Tx) error) (err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx := context.Background()
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return err
	}
	defer release()

	var priorSync int
	if err := conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&priorSync); err != nil {
		return fmt.Errorf("read synchronous mode: %w", err)
	}
	if priorSync != 2 {
		if err := setResolveStateSynchronous(ctx, conn, 2); err != nil {
			return fmt.Errorf("enable FULL synchronous mode: %w", err)
		}
		defer func() {
			if restoreErr := setResolveStateSynchronous(ctx, conn, priorSync); restoreErr != nil {
				err = errors.Join(err, fmt.Errorf("restore synchronous mode %d: %w", priorSync, restoreErr))
			}
		}()
	}

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := apply(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func setResolveStateSynchronous(ctx context.Context, conn *sql.Conn, level int) error {
	var statement string
	switch level {
	case 0:
		statement = `PRAGMA synchronous = OFF`
	case 1:
		statement = `PRAGMA synchronous = NORMAL`
	case 2:
		statement = `PRAGMA synchronous = FULL`
	case 3:
		statement = `PRAGMA synchronous = EXTRA`
	default:
		return fmt.Errorf("unsupported SQLite synchronous mode %d", level)
	}
	_, err := conn.ExecContext(ctx, statement)
	return err
}
