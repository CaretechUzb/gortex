package store_sqlite

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

const (
	evictFilePredicate         = `file_path = ?`
	evictFilesPredicate        = `file_path IN (SELECT CAST(value AS TEXT) FROM json_each(?))`
	evictRepoPredicate         = `repo_prefix = ?`
	evictNonEmptyRepoPredicate = `repo_prefix = ? AND repo_prefix <> ''`
)

// EvictFiles removes a bounded file replacement set in one transaction. The
// two edge deletes deliberately use indexed from_id/to_id predicates instead
// of an OR expression, and keep the candidate node set inside SQLite rather
// than materialising every node ID in Go.
func (s *Store) EvictFiles(filePaths []string) (nodesRemoved, edgesRemoved int) {
	paths := make([]string, 0, len(filePaths))
	seen := make(map[string]struct{}, len(filePaths))
	for _, filePath := range filePaths {
		if filePath == "" {
			continue
		}
		if _, duplicate := seen[filePath]; duplicate {
			continue
		}
		seen[filePath] = struct{}{}
		paths = append(paths, filePath)
	}
	if len(paths) == 0 {
		return 0, 0
	}
	pathsJSON, ok := projectionJSON(paths)
	if !ok {
		return 0, 0
	}
	return s.evictByPredicate(evictFilesPredicate, pathsJSON)
}

// evictByPredicate is the common SQLite-native scope eviction path. The
// predicate is always one of the package constants above, never caller SQL.
func (s *Store) evictByPredicate(predicate string, arg any) (nodesRemoved, edgesRemoved int) {
	nodesRemoved, edgesRemoved, err := s.evictByPredicateResult(predicate, arg)
	if err != nil {
		panicOnFatal(err)
		return 0, 0
	}
	return nodesRemoved, edgesRemoved
}

// evictByPredicateResult keeps the entire binding/edge/node change in one
// IMMEDIATE transaction. Candidate node IDs remain in SQLite: the two indexed
// edge deletes consume the same predicate subquery directly, so scope size
// never creates a Go ID frontier or a DELETE-per-node loop.
func (s *Store) evictByPredicateResult(predicate string, arg any) (nodesRemoved, edgesRemoved int, retErr error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.beginWrite()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	ctx := context.Background()
	// Generation-unscoped on purpose, like the repo sweeps in store_purge.go:
	// this path also serves EvictRepo, and the node/edge deletes below reach
	// every generation, so leaving a binding row behind in another one would
	// strand it against nodes that no longer exist.
	if _, err := tx.ExecContext(ctx, `DELETE FROM semantic_binding_types WHERE `+predicate, arg); err != nil {
		return 0, 0, err
	}
	scopedNodes := `SELECT id FROM nodes WHERE ` + predicate
	for _, column := range []string{"from_id", "to_id"} {
		result, err := tx.ExecContext(ctx, `DELETE FROM edges WHERE `+column+` IN (`+scopedNodes+`)`, arg)
		if err != nil {
			return 0, 0, err
		}
		removed, err := result.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		edgesRemoved += int(removed)
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE `+predicate, arg)
	if err != nil {
		return 0, 0, err
	}
	removed, err := result.RowsAffected()
	if err != nil {
		return 0, 0, err
	}
	nodesRemoved = int(removed)
	changed := nodesRemoved > 0 || edgesRemoved > 0
	invalidatedAnalysis := false
	if changed && s.analysisGenerationPresent {
		if err := invalidateAnalysisGenerationTx(tx); err != nil {
			return 0, 0, err
		}
		invalidatedAnalysis = true
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}

	if invalidatedAnalysis {
		s.analysisGenerationPresent = false
	}
	s.finishAnalysisMutationLocked(changed)
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return nodesRemoved, edgesRemoved, nil
}

var _ graph.FileBatchEvicter = (*Store)(nil)
