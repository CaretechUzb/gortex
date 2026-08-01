package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

// ScanUnresolvedEdgeIdentitiesBatched keyset-pages only the logical identities
// of unresolved edges in the requested kinds. Each page cursor and the active
// writer connection gate are released before yield runs, allowing the callback
// to exact-refetch and rewrite the same store safely.
func (s *Store) ScanUnresolvedEdgeIdentitiesBatched(
	kinds []graph.EdgeKind,
	batchSize int,
	yield func([]graph.EdgeIdentity) bool,
) {
	if yield == nil {
		return
	}
	kindValues := sqliteEdgeBatchKinds(kinds)
	if len(kindValues) == 0 {
		return
	}
	if batchSize <= 0 {
		batchSize = 1
	}
	if batchSize > maxEdgeKindScanBatch {
		batchSize = maxEdgeKindScanBatch
	}
	highWater, ok := s.edgeKindHighWater(kindValues)
	if !ok || highWater == 0 {
		return
	}

	var after int64
	for after < highWater {
		identities, next, ok := s.unresolvedEdgeIdentityPage(
			kindValues, after, highWater, batchSize,
		)
		if !ok || len(identities) == 0 || next <= after {
			return
		}
		after = next
		if !yield(identities) {
			return
		}
	}
}

func (s *Store) unresolvedEdgeIdentityPage(
	kinds []string,
	after, highWater int64,
	limit int,
) ([]graph.EdgeIdentity, int64, bool) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	query := `SELECT id, from_id, to_id, kind, file_path, line
	FROM edges NOT INDEXED
	WHERE id > ? AND id <= ? AND kind IN (` + inPlaceholders(len(kinds)) + `)
	  AND (substr(to_id, 1, 12) = 'unresolved::'
	       OR instr(to_id, '::unresolved::') > 0)
	ORDER BY id
	LIMIT ?`
	args := make([]any, 0, len(kinds)+3)
	args = append(args, after, highWater)
	args = append(args, toAnyArgs(kinds)...)
	args = append(args, limit)
	rows, err := s.queryActiveWriteLocked(context.Background(), query, args...)
	if err != nil {
		panicOnFatal(err)
		return nil, after, false
	}
	defer rows.Close()

	identities := make([]graph.EdgeIdentity, 0, limit)
	next := after
	for rows.Next() {
		var (
			rowID    int64
			identity graph.EdgeIdentity
		)
		if err := rows.Scan(
			&rowID,
			&identity.From,
			&identity.To,
			&identity.Kind,
			&identity.FilePath,
			&identity.Line,
		); err != nil {
			panicOnFatal(err)
			return nil, after, false
		}
		if rowID > next {
			next = rowID
		}
		identities = append(identities, identity)
	}
	if err := rows.Err(); err != nil {
		panicOnFatal(err)
		return nil, after, false
	}
	return identities, next, true
}

var _ graph.UnresolvedEdgeIdentityBatchScanner = (*Store)(nil)
