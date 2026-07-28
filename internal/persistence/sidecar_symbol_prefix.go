package persistence

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

// prefixSymbolIDsMark is the migration_marks kind for the one-shot rewrite
// below. Per repo_key, so a workspace migrates each repo exactly once.
const prefixSymbolIDsMark = "prefix-symbol-ids"

// SymbolIDRewriter maps a stored symbol anchor to its current form, or
// returns "" to leave it untouched.
//
// The caller owns the decision, because only it can see the graph: the
// daemon's implementation returns a rewritten id ONLY when the old id names
// no live node and the prefixed candidate does. That asymmetry is what makes
// this safe to run against user data — an anchor that still resolves is never
// touched, and one that resolves nowhere is left alone rather than guessed at.
type SymbolIDRewriter func(oldID string) string

// PrefixSymbolIDs rewrites the symbol anchors stored for one repo, once.
//
// Notes, memories, notebooks and suppressions all anchor to symbol ids, and
// surfacing matches those ids against the caller's by string equality. Any
// change to id SHAPE therefore severs every stored anchor at once and returns
// nothing — indistinguishable, from the outside, from "no memories are
// relevant". Making repo prefixes unconditional is exactly such a change:
// `internal/foo.go::Bar` became `gortex/internal/foo.go::Bar`.
//
// Idempotent via migration_marks, and a no-op once marked. Returns the number
// of rows actually rewritten.
//
// Runs in one IMMEDIATE transaction: a partial rewrite would leave a repo's
// memories split across two id conventions, which is worse than either.
func (s *SidecarStore) PrefixSymbolIDs(repoKey string, rewrite SymbolIDRewriter) (int, error) {
	if s == nil || s.db == nil || repoKey == "" || rewrite == nil {
		return 0, nil
	}
	if s.migrationDone(repoKey, prefixSymbolIDsMark) {
		return 0, nil
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("persistence: prefix symbol ids: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// Every anchor table is WITHOUT ROWID, so updates key on the table's own
	// primary-key column rather than a rowid.
	total := 0
	for _, t := range []struct{ table, column, key string }{
		{"notes", "symbol_id", "id"},
		{"suppressions", "symbol_id", "identity_key"},
	} {
		n, err := rewriteScalarSymbolColumn(tx, t.table, t.column, t.key, repoKey, rewrite)
		if err != nil {
			return 0, err
		}
		total += n
	}
	for _, t := range []struct{ table, column, key string }{
		{"memories", "symbol_ids", "id"},
		// auto_links holds symbol ids the auto-linker derived from a
		// memory's body; they anchor the same way symbol_ids do and go
		// stale identically.
		{"memories", "auto_links", "id"},
		{"notebooks", "symbol_ids", "id"},
	} {
		n, err := rewriteJSONSymbolColumn(tx, t.table, t.column, t.key, repoKey, rewrite)
		if err != nil {
			return 0, err
		}
		total += n
	}

	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO migration_marks (repo_key, kind, done_at) VALUES (?,?,?)`,
		repoKey, prefixSymbolIDsMark, time.Now().UTC().UnixNano()); err != nil {
		return 0, fmt.Errorf("persistence: prefix symbol ids: mark: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("persistence: prefix symbol ids: commit: %w", err)
	}
	return total, nil
}

// rewriteScalarSymbolColumn rewrites a single-id TEXT column.
func rewriteScalarSymbolColumn(tx *sql.Tx, table, column, keyCol, repoKey string, rewrite SymbolIDRewriter) (int, error) {
	rows, err := tx.Query(
		`SELECT `+keyCol+`, `+column+` FROM `+table+` WHERE repo_key = ? AND `+column+` <> ''`, repoKey)
	if err != nil {
		return 0, fmt.Errorf("persistence: prefix symbol ids: read %s.%s: %w", table, column, err)
	}
	type update struct {
		key string
		val string
	}
	var updates []update
	for rows.Next() {
		var key, id string
		if err := rows.Scan(&key, &id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("persistence: prefix symbol ids: scan %s.%s: %w", table, column, err)
		}
		if next := rewrite(id); next != "" && next != id {
			updates = append(updates, update{key, next})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, u := range updates {
		if _, err := tx.Exec(`UPDATE `+table+` SET `+column+` = ? WHERE repo_key = ? AND `+keyCol+` = ?`, u.val, repoKey, u.key); err != nil {
			return 0, fmt.Errorf("persistence: prefix symbol ids: write %s.%s: %w", table, column, err)
		}
	}
	return len(updates), nil
}

// rewriteJSONSymbolColumn rewrites a JSON-array-of-ids TEXT column. A value
// that does not decode as a string array is left exactly as it is: this runs
// over user data, so an unrecognised shape is skipped rather than replaced.
func rewriteJSONSymbolColumn(tx *sql.Tx, table, column, keyCol, repoKey string, rewrite SymbolIDRewriter) (int, error) {
	rows, err := tx.Query(
		`SELECT `+keyCol+`, `+column+` FROM `+table+` WHERE repo_key = ?`, repoKey)
	if err != nil {
		return 0, fmt.Errorf("persistence: prefix symbol ids: read %s.%s: %w", table, column, err)
	}
	type update struct {
		key string
		val string
	}
	var updates []update
	for rows.Next() {
		var key, raw string
		if err := rows.Scan(&key, &raw); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("persistence: prefix symbol ids: scan %s.%s: %w", table, column, err)
		}
		var ids []string
		if err := json.Unmarshal([]byte(raw), &ids); err != nil || len(ids) == 0 {
			continue
		}
		changed := false
		for i, id := range ids {
			if next := rewrite(id); next != "" && next != id {
				ids[i] = next
				changed = true
			}
		}
		if !changed {
			continue
		}
		encoded, err := json.Marshal(ids)
		if err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("persistence: prefix symbol ids: encode %s.%s: %w", table, column, err)
		}
		updates = append(updates, update{key, string(encoded)})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, u := range updates {
		if _, err := tx.Exec(`UPDATE `+table+` SET `+column+` = ? WHERE repo_key = ? AND `+keyCol+` = ?`, u.val, repoKey, u.key); err != nil {
			return 0, fmt.Errorf("persistence: prefix symbol ids: write %s.%s: %w", table, column, err)
		}
	}
	return len(updates), nil
}
