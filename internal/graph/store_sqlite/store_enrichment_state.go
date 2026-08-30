package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion that the SQLite Store persists the enrichment
// completion marker. Lifting this state into the same backend the graph
// lives in lets a warm restart skip re-enriching a repo whose persisted
// graph already carries its LSP edges — no second persistence surface.
var _ graph.EnrichmentStateStore = (*Store)(nil)

// SetEnrichmentState upserts the git-revision completion marker for one
// (repo, provider) — written when a provider finishes a non-partial pass on a
// clean tree. One row per (repo_prefix, provider).
//
// An upsert rather than INSERT OR REPLACE, and the difference is load-bearing:
// REPLACE deletes the row and reinserts it, so the two readiness columns this
// statement does not name — gen and content_gen — would silently fall back to
// their DEFAULT 0. A provider that had just recorded a completed pass would
// read "never ran" the moment it recorded its sha. Those columns are owned by
// CompleteEnrichmentProvider and are deliberately untouched here.
func (s *Store) SetEnrichmentState(st graph.EnrichmentState) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.execActiveWriteLocked(context.Background(), `
INSERT INTO enrichment_state
  (repo_prefix, provider, indexed_sha, completed_at, coverage)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(repo_prefix, provider) DO UPDATE SET
  indexed_sha  = excluded.indexed_sha,
  completed_at = excluded.completed_at,
  coverage     = excluded.coverage`,
		st.RepoPrefix, st.Provider, st.IndexedSHA, st.CompletedAt, st.Coverage)
	return err
}

// GetEnrichmentState returns the recorded completion marker for a
// (repo, provider). The bool is false when no row exists yet (never-enriched
// or pre-feature).
func (s *Store) GetEnrichmentState(repoPrefix, provider string) (graph.EnrichmentState, bool, error) {
	row := s.db.QueryRow(`
SELECT indexed_sha, completed_at, coverage
  FROM enrichment_state WHERE repo_prefix = ? AND provider = ?`, repoPrefix, provider)
	st := graph.EnrichmentState{RepoPrefix: repoPrefix, Provider: provider}
	err := row.Scan(&st.IndexedSHA, &st.CompletedAt, &st.Coverage)
	if err == sql.ErrNoRows {
		return graph.EnrichmentState{RepoPrefix: repoPrefix, Provider: provider}, false, nil
	}
	if err != nil {
		return graph.EnrichmentState{RepoPrefix: repoPrefix, Provider: provider}, false, err
	}
	return st, true, nil
}
