package store_sqlite

import (
	"context"
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion that the SQLite Store models provider applicability,
// not just provider completion.
var _ graph.EnrichmentApplicabilityStore = (*Store)(nil)

// DeclareEnrichmentProviders makes providers the authoritative applicable set
// for one repo.
//
// Rows are the applicability model, and this is where they are written. A
// provider that applies but has never run must be a VISIBLE row at content_gen
// 0, because readiness takes the minimum across a repo's providers: if absence
// meant "not applicable", a repo with python-types current and go-types never
// started would read fully enriched, one fresh provider masking a sibling that
// never began.
//
// The set is authoritative in both directions. Rows for providers no longer in
// it are dropped -- a repo whose last Python file was deleted must stop being
// judged on python-types, or it reads "partial" forever with no way to clear
// it. That is why the caller must derive the set from POSITIVE evidence (the
// languages actually present in this repo's nodes) and must not call this at
// all when it has no evidence either way; an empty set here means "no provider
// applies", not "I did not look".
func (s *Store) DeclareEnrichmentProviders(repoPrefix string, providers []string) error {
	if repoPrefix == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	for _, provider := range providers {
		if provider == "" {
			continue
		}
		// OR IGNORE, never REPLACE: a provider that has already completed keeps
		// its stamp. Re-declaring an existing row must not reset a real pass to
		// "never ran".
		if _, err := tx.Exec(`
INSERT OR IGNORE INTO enrichment_state (repo_prefix, provider, content_gen)
VALUES (?, ?, 0)`, repoPrefix, provider); err != nil {
			return err
		}
	}

	// Drop rows outside the declared set, leaving the two sentinels alone --
	// they are rollups, not providers, and are managed just below.
	del := `DELETE FROM enrichment_state WHERE repo_prefix = ? AND provider NOT IN (?, ?`
	args := []any{repoPrefix, graph.EnrichProviderRepoMarker, graph.EnrichProviderNone}
	for _, provider := range providers {
		if provider == "" {
			continue
		}
		del += `, ?`
		args = append(args, provider)
	}
	del += `)`
	if _, err := tx.Exec(del, args...); err != nil {
		return err
	}

	if len(providers) == 0 {
		if _, err := tx.Exec(`
INSERT OR IGNORE INTO enrichment_state (repo_prefix, provider, content_gen)
VALUES (?, ?, 0)`, repoPrefix, graph.EnrichProviderNone); err != nil {
			return err
		}
	} else if _, err := tx.Exec(
		`DELETE FROM enrichment_state WHERE repo_prefix = ? AND provider = ?`,
		repoPrefix, graph.EnrichProviderNone); err != nil {
		return err
	}

	return tx.Commit()
}

// CompleteEnrichmentProvider records that one provider finished a pass over
// this repo's content as of contentGen.
//
// contentGen is supplied by the caller, unlike the derive's stamp which the
// store reads for itself. The difference is a real one, not an inconsistency:
// a derive holds the batch-mutation write gate for its whole run, so the
// counter cannot move under it and reading at completion is exact. An
// enrichment pass holds nothing -- the watcher can reindex a file while gopls
// is still hovering -- so the honest value is the one observed BEFORE the pass
// started, and only the caller was there to see it.
//
// Two guards keep a caller-supplied number from lying. MIN against the current
// counter means a caller can never claim content that does not exist yet; MAX
// against the stored value means a slow pass finishing after a fast one cannot
// walk the row backwards.
func (s *Store) CompleteEnrichmentProvider(repoPrefix, provider string, contentGen int64) error {
	if repoPrefix == "" || provider == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // rollback after Commit is a no-op

	if _, err := tx.Exec(`
INSERT INTO enrichment_state (repo_prefix, provider, gen, content_gen)
VALUES (
  ?, ?,
  COALESCE((SELECT gen FROM repo_graph_gen WHERE repo_prefix = ?), 0),
  MIN(?, COALESCE((SELECT content_gen FROM repo_graph_gen WHERE repo_prefix = ?), 0)))
ON CONFLICT(repo_prefix, provider) DO UPDATE SET
  gen         = excluded.gen,
  content_gen = MAX(enrichment_state.content_gen, excluded.content_gen)`,
		repoPrefix, provider, repoPrefix, contentGen, repoPrefix); err != nil {
		return err
	}

	// A real provider ran, so "no provider applies" is now false. Clearing it
	// here as well as in DeclareEnrichmentProviders covers the repo that gained
	// its first Go or Python file between two declarations.
	if provider != graph.EnrichProviderNone && provider != graph.EnrichProviderRepoMarker {
		if _, err := tx.Exec(
			`DELETE FROM enrichment_state WHERE repo_prefix = ? AND provider = ?`,
			repoPrefix, graph.EnrichProviderNone); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// RepoContentGen reads one repo's content counter. Zero for a repo with no row:
// nothing has been indexed for that prefix, which is the honest starting point.
func (s *Store) RepoContentGen(repoPrefix string) (int64, error) {
	var gen int64
	err := s.db.QueryRow(
		`SELECT content_gen FROM repo_graph_gen WHERE repo_prefix = ?`, repoPrefix).Scan(&gen)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return gen, nil
}

// EnrichmentContentGens returns the recorded content_gen for each of a repo's
// provider rows, sentinels included so a caller can tell "no provider applies"
// from "no provider has ever been looked at".
func (s *Store) EnrichmentContentGens(repoPrefix string) (map[string]int64, error) {
	rows, err := s.db.Query(
		`SELECT provider, content_gen FROM enrichment_state WHERE repo_prefix = ?`, repoPrefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck // read-only cursor
	out := make(map[string]int64)
	for rows.Next() {
		var provider string
		var gen int64
		if err := rows.Scan(&provider, &gen); err != nil {
			return nil, err
		}
		out[provider] = gen
	}
	return out, rows.Err()
}

// DeclareNoEnrichmentProvidersIfUnrecorded writes the EnrichProviderNone
// sentinel for a repo that has NO enrichment rows at all, and does nothing for
// one that has any.
//
// The caller for this is the indexer's "this build has no semantic providers"
// path, which is a weaker signal than the enrichment pass's own census: a
// provider registry that is empty right now may simply not have been populated
// yet. Declaring an empty set through DeclareEnrichmentProviders there would
// delete real completions, and nothing would restore them — the very condition
// that triggered the call is the one that stops the pass from running again. So
// this variant can only ever add information, never destroy it.
func (s *Store) DeclareNoEnrichmentProvidersIfUnrecorded(repoPrefix string) error {
	if repoPrefix == "" {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err := s.execActiveWriteLocked(context.Background(), `
INSERT OR IGNORE INTO enrichment_state (repo_prefix, provider, content_gen)
SELECT ?, ?, 0
 WHERE NOT EXISTS (SELECT 1 FROM enrichment_state WHERE repo_prefix = ?)`,
		repoPrefix, graph.EnrichProviderNone, repoPrefix)
	return err
}
