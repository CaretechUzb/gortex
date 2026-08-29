package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// EvictEdgesByKinds removes a derived edge generation with one SQLite DELETE.
// Reconciliation uses this before publishing a replacement batch, replacing
// thousands of individual transactions and analysis-generation invalidations.
func (s *Store) EvictEdgesByKinds(kinds []graph.EdgeKind) int {
	if len(kinds) == 0 {
		return 0
	}
	values := make([]string, 0, len(kinds))
	for _, kind := range kinds {
		values = append(values, string(kind))
	}
	kindsJSON, ok := projectionJSON(values)
	if !ok {
		return 0
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if !s.invalidateAnalysisBeforeMutationLocked() {
		return 0
	}
	tx, err := s.beginWrite()
	if err != nil {
		panicOnFatal(err)
		return 0
	}
	res, err := tx.Exec(`
DELETE FROM edges
WHERE kind IN (SELECT CAST(value AS TEXT) FROM json_each(?))`, kindsJSON)
	if err != nil {
		_ = tx.Rollback()
		panicOnFatal(err)
		return 0
	}
	removed64, err := res.RowsAffected()
	if err != nil {
		_ = tx.Rollback()
		panicOnFatal(err)
		return 0
	}
	// An edge-kind eviction deletes across every repository at once and never
	// enumerates the rows it removed, so it cannot name the repos it touched.
	changed := removed64 > 0
	if err := s.commitGraphMutation(tx, changed, nil, true); err != nil {
		panicOnFatal(err)
		return 0
	}
	if changed {
		s.markMutationReceiptsIncompleteLocked()
	}
	return int(removed64)
}

var _ graph.EdgeKindEvicter = (*Store)(nil)
