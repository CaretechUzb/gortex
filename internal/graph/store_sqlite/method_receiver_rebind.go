package store_sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

var (
	_ graph.GoMethodReceiverRebinder      = (*Store)(nil)
	_ graph.GoMethodReceiverBatchRebinder = (*Store)(nil)
)

// The global candidate query is deliberately driven by the narrow kind index.
// It scans only member_of rowids and projects (edge rowid, canonical target)
// into a temp table; no graph rows cross the SQLite/Go boundary. Scoped
// warm/partial paths remain driven by edges_by_from below.
const goMethodReceiverCandidatesGlobalSQL = `
SELECT e.id, MIN(c.id)
FROM edges AS e INDEXED BY edges_by_kind
JOIN nodes AS m ON m.id = e.from_id
LEFT JOIN nodes AS t ON t.id = e.to_id
JOIN nodes AS c INDEXED BY nodes_go_receiver_type
  ON c.repo_prefix = m.repo_prefix
 AND c.file_dir = e.member_receiver_dir
 AND c.name = e.member_receiver
WHERE e.kind = 'member_of'
  AND e.member_receiver IS NOT NULL
  AND e.member_receiver_dir IS NOT NULL
  AND m.language = 'go'
  AND m.kind = 'method'
  AND (t.id IS NULL OR t.kind NOT IN ('type', 'interface'))
  AND c.language = 'go'
  AND c.kind IN ('type', 'interface')
  AND c.name <> ''
  AND c.file_path <> ''
GROUP BY e.id
HAVING COUNT(*) = 1 AND MIN(c.id) <> e.to_id`

// The scoped sibling starts from nodes_by_file and reaches member_of through
// edges_by_from. This matters for partial indexing: repeatedly scanning the
// global member index once per changed file would turn the streaming tail back
// into O(files*all_methods).
const goMethodReceiverCandidatesForFileSQL = `
SELECT e.id, MIN(c.id)
FROM nodes AS m INDEXED BY nodes_by_file
JOIN edges AS e INDEXED BY edges_by_from
  ON e.from_id = m.id
 AND e.kind = 'member_of'
LEFT JOIN nodes AS t ON t.id = e.to_id
JOIN nodes AS c INDEXED BY nodes_go_receiver_type
  ON c.repo_prefix = m.repo_prefix
 AND c.file_dir = e.member_receiver_dir
 AND c.name = e.member_receiver
WHERE m.file_path = ?
  AND m.language = 'go'
  AND m.kind = 'method'
  AND e.member_receiver IS NOT NULL
  AND e.member_receiver_dir IS NOT NULL
  AND (t.id IS NULL OR t.kind NOT IN ('type', 'interface'))
  AND c.language = 'go'
  AND c.kind IN ('type', 'interface')
  AND c.name <> ''
  AND c.file_path <> ''
GROUP BY e.id
HAVING COUNT(*) = 1 AND MIN(c.id) <> e.to_id`

const goMethodReceiverCandidateTableSQL = `CREATE TEMP TABLE IF NOT EXISTS go_receiver_rebind_candidates (
    edge_id INTEGER PRIMARY KEY,
    new_to  TEXT NOT NULL
) WITHOUT ROWID`

// RebindGoMethodReceivers repairs phantom Go receiver targets entirely inside
// SQLite. filePath="" covers the whole graph; a non-empty path limits the
// source methods to that file for incremental/partial indexing.
//
// The temp table is populated once under writeMu, then the mutation transaction
// handles logical-key collisions with the same semantics as ReindexEdges:
// an already-existing canonical edge wins, and when multiple old rows collapse
// onto one canonical key the lowest old edge id (the original scan order)
// supplies the surviving payload. Every candidate is therefore either updated
// or deliberately removed; `changed` is the number of old identities consumed.
func (s *Store) RebindGoMethodReceivers(filePath string) (changed int, err error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	ctx := context.Background()
	conn, release, err := s.activeWriteConnLocked(ctx)
	if err != nil {
		return 0, fmt.Errorf("sqlite receiver rebind acquire writer connection: %w", err)
	}
	defer release()

	if _, err = conn.ExecContext(ctx, goMethodReceiverCandidateTableSQL); err != nil {
		return 0, fmt.Errorf("sqlite receiver rebind create candidate table: %w", err)
	}
	if _, err = conn.ExecContext(ctx, `DELETE FROM temp.go_receiver_rebind_candidates`); err != nil {
		return 0, fmt.Errorf("sqlite receiver rebind clear candidate table: %w", err)
	}
	// A pooled connection retains TEMP tables. Leave it empty when returned to
	// the pool so a diagnostic connection never sees stale candidate IDs.
	defer func() {
		_, _ = conn.ExecContext(context.Background(), `DELETE FROM temp.go_receiver_rebind_candidates`)
	}()

	insertSQL := `INSERT INTO temp.go_receiver_rebind_candidates (edge_id, new_to) ` +
		goMethodReceiverCandidatesGlobalSQL
	var result sql.Result
	if filePath == "" {
		result, err = conn.ExecContext(ctx, insertSQL)
	} else {
		insertSQL = `INSERT INTO temp.go_receiver_rebind_candidates (edge_id, new_to) ` +
			goMethodReceiverCandidatesForFileSQL
		result, err = conn.ExecContext(ctx, insertSQL, filePath)
	}
	if err != nil {
		return 0, fmt.Errorf("sqlite receiver rebind collect candidates: %w", err)
	}
	candidates, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sqlite receiver rebind count candidates: %w", err)
	}
	if candidates == 0 {
		return 0, nil
	}

	tx, err := s.beginWriteOnConnContext(ctx, conn)
	if err != nil {
		return 0, fmt.Errorf("sqlite receiver rebind begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Invalidate durable whole-graph analysis in the same transaction and on
	// the same pinned connection as the topology rewrite. Besides making the
	// crash invariant atomic, this avoids checking out a second connection and
	// hanging when the SQLite pool is intentionally limited to one connection.
	analysisInvalidated := s.analysisGenerationPresent
	if analysisInvalidated {
		if err = invalidateAnalysisGenerationTx(tx); err != nil {
			return 0, fmt.Errorf("sqlite receiver rebind invalidate analysis: %w", err)
		}
	}

	// ReindexEdges deletes an old identity and INSERT OR IGNOREs the new one.
	// If that new identity already exists, its row/payload survives and the old
	// row disappears. Reproduce that behavior before the in-place UPDATE.
	if _, err = tx.ExecContext(ctx, `
DELETE FROM edges
WHERE id IN (
    SELECT old.id
    FROM edges AS old
    JOIN temp.go_receiver_rebind_candidates AS r ON r.edge_id = old.id
    WHERE EXISTS (
        SELECT 1
        FROM edges AS existing
        WHERE existing.id <> old.id
          AND existing.from_id = old.from_id
          AND existing.to_id = r.new_to
          AND existing.kind = old.kind
          AND existing.file_path = old.file_path
          AND existing.line = old.line
    )
)`); err != nil {
		return 0, fmt.Errorf("sqlite receiver rebind remove existing-key conflicts: %w", err)
	}

	// Multiple phantom targets can collapse onto the same logical edge. Keep
	// the lowest row id deterministically, matching SQLite's kind-index scan
	// order used by the previous ReindexEdges implementation.
	if _, err = tx.ExecContext(ctx, `
DELETE FROM edges
WHERE id IN (
    SELECT edge_id
    FROM (
        SELECT old.id AS edge_id,
               ROW_NUMBER() OVER (
                   PARTITION BY old.from_id, r.new_to, old.kind, old.file_path, old.line
                   ORDER BY old.id
               ) AS duplicate_rank
        FROM edges AS old
        JOIN temp.go_receiver_rebind_candidates AS r ON r.edge_id = old.id
    )
    WHERE duplicate_rank > 1
)`); err != nil {
		return 0, fmt.Errorf("sqlite receiver rebind deduplicate canonical keys: %w", err)
	}

	if _, err = tx.ExecContext(ctx, `
UPDATE edges
SET to_id = (
    SELECT r.new_to
    FROM temp.go_receiver_rebind_candidates AS r
    WHERE r.edge_id = edges.id
)
WHERE id IN (SELECT edge_id FROM temp.go_receiver_rebind_candidates)`); err != nil {
		return 0, fmt.Errorf("sqlite receiver rebind update targets: %w", err)
	}
	if err = s.commitGraphMutation(
		tx, true, []string{graph.RepoPrefixOfID(filePath)}, false,
	); err != nil {
		return 0, fmt.Errorf("sqlite receiver rebind commit: %w", err)
	}
	committed = true
	if analysisInvalidated {
		s.analysisGenerationPresent = false
	}
	return int(candidates), nil
}
