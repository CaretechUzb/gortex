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
INSERT OR IGNORE INTO enrichment_state (view_gen, repo_prefix, provider, content_gen)
VALUES (?, ?, ?, 0)`, s.viewGen, repoPrefix, provider); err != nil {
			return err
		}
	}

	// Drop rows outside the declared set, leaving the two sentinels alone --
	// they are rollups, not providers, and are managed just below.
	//
	// Checkout-scoped MARKERS are spared for a different reason: they are not
	// applicability rows at all. Their key is "<provider>@<checkout>", one per
	// working copy, and no declaration ever names them -- so without this
	// clause the prune deleted a sibling checkout's marker every time another
	// checkout of the same family was enriched, and each pass re-enriched a
	// tree the store had already covered.
	del := `DELETE FROM enrichment_state
 WHERE view_gen = ? AND repo_prefix = ?
   AND instr(provider, ?) = 0
   AND provider NOT IN (?, ?`
	args := []any{
		s.viewGen, repoPrefix,
		graph.EnrichCheckoutMarkerSeparator,
		graph.EnrichProviderRepoMarker, graph.EnrichProviderNone,
	}
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
INSERT OR IGNORE INTO enrichment_state (view_gen, repo_prefix, provider, content_gen)
VALUES (?, ?, ?, 0)`, s.viewGen, repoPrefix, graph.EnrichProviderNone); err != nil {
			return err
		}
	} else if _, err := tx.Exec(
		`DELETE FROM enrichment_state WHERE view_gen = ? AND repo_prefix = ? AND provider = ?`,
		s.viewGen, repoPrefix, graph.EnrichProviderNone); err != nil {
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
INSERT INTO enrichment_state (view_gen, repo_prefix, provider, gen, content_gen)
VALUES (
  ?, ?, ?,
  COALESCE((SELECT gen FROM repo_graph_gen WHERE repo_prefix = ?), 0),
  MIN(?, COALESCE((SELECT content_gen FROM repo_graph_gen WHERE repo_prefix = ?), 0)))
ON CONFLICT(view_gen, repo_prefix, provider) DO UPDATE SET
  gen         = excluded.gen,
  content_gen = MAX(enrichment_state.content_gen, excluded.content_gen)`,
		s.viewGen, repoPrefix, provider, repoPrefix, contentGen, repoPrefix); err != nil {
		return err
	}

	// A real provider ran, so "no provider applies" is now false. Clearing it
	// here as well as in DeclareEnrichmentProviders covers the repo that gained
	// its first Go or Python file between two declarations.
	if provider != graph.EnrichProviderNone && provider != graph.EnrichProviderRepoMarker {
		if _, err := tx.Exec(
			`DELETE FROM enrichment_state WHERE view_gen = ? AND repo_prefix = ? AND provider = ?`,
			s.viewGen, repoPrefix, graph.EnrichProviderNone); err != nil {
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
		`SELECT provider, content_gen FROM enrichment_state WHERE view_gen = ? AND repo_prefix = ?`,
		s.viewGen, repoPrefix)
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

// EnrichmentNeverRan reports whether this repo is owed a pass nothing has ever
// run. Two arms, both point lookups on the primary key:
//
//	no rows at all         nobody has even asked which providers apply
//	a provider at gen = 0  declared applicable, never completed a pass
//
// gen is the right column and content_gen is not. A dirty tree never records a
// sha, so indexed_sha stays empty on a repo whose providers have completed
// many times, and content_gen legitimately reads 0 on a repo whose content
// counter has not moved. gen is written only by CompleteEnrichmentProvider,
// from the repo's live counter, so a non-zero value means a pass really
// finished — and it can never go back.
//
// The sentinels are excluded because neither is a provider: __none__ is the
// pass having looked and found nothing applicable, and __repo__ is the
// whole-repo rollup, which CompleteEnrichmentProvider is never called for.
func (s *Store) EnrichmentNeverRan(repoPrefix string) (bool, error) {
	if repoPrefix == "" {
		return false, nil
	}
	var owed bool
	err := s.db.QueryRow(`
SELECT NOT EXISTS(SELECT 1 FROM enrichment_state WHERE view_gen = ? AND repo_prefix = ?)
    OR EXISTS(SELECT 1 FROM enrichment_state
               WHERE view_gen = ? AND repo_prefix = ? AND provider NOT IN (?, ?) AND gen = 0)`,
		s.viewGen, repoPrefix, s.viewGen, repoPrefix,
		graph.EnrichProviderRepoMarker, graph.EnrichProviderNone).Scan(&owed)
	if err != nil {
		return false, err
	}
	return owed, nil
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
INSERT OR IGNORE INTO enrichment_state (view_gen, repo_prefix, provider, content_gen)
SELECT ?, ?, ?, 0
 WHERE NOT EXISTS (SELECT 1 FROM enrichment_state WHERE view_gen = ? AND repo_prefix = ?)`,
		s.viewGen, repoPrefix, graph.EnrichProviderNone, s.viewGen, repoPrefix)
	return err
}

// RefreshEnrichmentProviders declares every provider that has ALREADY completed
// a pass for this repo current as of the repo's content counter, and creates
// nothing.
//
// The caller is the warm-restart gate that decides a repo needs no enrichment
// at all because its whole-repo completion marker already records this clean
// HEAD. That decision is the system asserting every applicable provider
// finished for this revision; if it is trusted enough to skip minutes of hover
// work, it is trusted enough to record what it certified.
//
// Without it, every store that carried enrichment rows written before the
// content stamp existed would read "partial" the moment its legacy derive row
// cleared — and stay there, because the very gate that would refresh it is the
// one deciding not to run. A permanent false alarm on every upgraded install is
// how a readiness column teaches people to ignore it.
//
// Two guards make this a renewal rather than an invention. indexed_sha <> ”
// admits only providers that really completed a pass at some revision, so a row
// declared applicable and never run keeps its zero — laundering that is the one
// thing the applicability model exists to prevent. And the sentinels are
// excluded: they are rollups, not providers.
func (s *Store) RefreshEnrichmentProviders(repoPrefix string) (int, error) {
	if repoPrefix == "" {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.execActiveWriteLocked(context.Background(), `
UPDATE enrichment_state
   SET content_gen = COALESCE((SELECT content_gen FROM repo_graph_gen WHERE repo_prefix = ?), 0),
       gen         = COALESCE((SELECT gen         FROM repo_graph_gen WHERE repo_prefix = ?), 0)
 WHERE view_gen = ? AND repo_prefix = ? AND provider NOT IN (?, ?) AND indexed_sha <> ''`,
		repoPrefix, repoPrefix, s.viewGen, repoPrefix,
		graph.EnrichProviderRepoMarker, graph.EnrichProviderNone)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}

// AdvanceContentGenForCompletedProviders renews the content stamp on providers
// that have already completed a pass, for a repo whose enrichment was refreshed
// over an exact FILE FRONTIER rather than the whole tree.
//
// The caller is the indexer's file-scoped deferred pass. That pass runs real
// provider work and used to record nothing, on the reasoning that a frontier
// cannot publish the whole-repository completion MARKER. True of the marker,
// which asserts every file was enriched at a SHA; false of the content COUNTER,
// which asserts only "this provider is current as of generation N". A fully
// discharged frontier is exactly that evidence, so withholding the counter left
// every actively-edited repo reading "partial" from its first save onward.
//
// Three guards, and none is decoration:
//
// gen > 0 is the one that must never be relaxed. gen is written only by
// CompleteEnrichmentProvider, from the repo's live counter, so a non-zero value
// means some pass really finished; a declared-but-never-run provider keeps its
// zero, and EnrichmentNeverRan reads that zero to re-arm the repo. Advancing
// content_gen on such a row would leave the re-arm intact, but writing gen
// would destroy it -- which is why this statement never writes gen at all, and
// why the obvious implementation (loop the rows through
// CompleteEnrichmentProvider, whose ON CONFLICT sets gen = excluded.gen
// unconditionally) is not available: it would let a repo claim repo-wide
// coverage from a one-file pass and never re-arm again.
//
// The MIN/MAX pair is CompleteEnrichmentProvider's clamp, copied deliberately
// rather than reinvented: MIN against the live counter so a caller cannot claim
// content that does not exist yet, MAX against the stored value so a slow pass
// finishing after a fast one cannot walk a row backwards.
//
// excludeProviders is the caller's list of providers whose language WAS in the
// frontier but which returned no result -- an unavailable provider, most often.
// Those rows must not advance: the re-parse evicted their edges and nothing
// restored them, so stamping would produce a repo that reads ready while
// silently missing edges. Holding one row down is the honest reading.
//
// Deliberately NOT merged with RefreshEnrichmentProviders, which bumps gen and
// filters on indexed_sha <> ” -- a filter that excludes exactly the dirty-tree
// and non-git population this path exists to serve.
func (s *Store) AdvanceContentGenForCompletedProviders(repoPrefix string, contentGen int64, excludeProviders []string) (int, error) {
	if repoPrefix == "" {
		return 0, nil
	}
	query := `
UPDATE enrichment_state
   SET content_gen = MAX(
         enrichment_state.content_gen,
         MIN(?, COALESCE((SELECT content_gen FROM repo_graph_gen WHERE repo_prefix = ?), 0)))
 WHERE view_gen = ?
   AND repo_prefix = ?
   AND provider NOT IN (?, ?)
   AND gen > 0`
	args := []any{
		contentGen, repoPrefix, s.viewGen, repoPrefix,
		graph.EnrichProviderRepoMarker, graph.EnrichProviderNone,
	}
	// One placeholder per excluded provider. Built here rather than passed as a
	// joined string because a provider name is caller data, and this statement
	// must stay parameterised end to end.
	if len(excludeProviders) > 0 {
		query += "\n   AND provider NOT IN (?"
		args = append(args, excludeProviders[0])
		for _, provider := range excludeProviders[1:] {
			query += ", ?"
			args = append(args, provider)
		}
		query += ")"
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	res, err := s.execActiveWriteLocked(context.Background(), query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return int(n), nil
}
