package store_sqlite

import (
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// StampDeriveState records that the global derived passes completed for each
// named repo, against that repo's counters as they stand right now.
//
// Both counters are read here, in the same transaction as the write, and
// neither is ever accepted from the caller. derived_content_gen is the one
// readiness compares; derived_gen rides along as provenance.
//
//	write path                    gen   content_gen   derived_content_gen
//	----------                    ---   -----------   -------------------
//	files indexed                  10             3                    --
//	derive's own edges land        25             3                    --
//	derive completes, stamps       25             3                     3
//	enrichment's edges land        40             3                     3
//	readiness: 3 >= 3                                              READY
//	file saved, mtime written      41             4                     3
//	readiness: 3 <  4                                            PARTIAL
//
// Stamping against gen instead of content_gen is the trap this shape exists to
// avoid: the derive's own edges, and every enrichment pass that follows it,
// advance gen, so a gen-stamped row is behind the moment the pipeline finishes
// and no later derive closes the gap -- a re-derive over unchanged content
// inserts nothing, so it moves nothing. Every repo would read "partial"
// forever after its very first successful pipeline.
//
// Reading content_gen at COMPLETION is exact rather than a concession. A
// derive holds the batch-mutation write gate for its entire run, so no content
// write can interleave and content_gen cannot move between start and
// completion. If that gate is ever weakened this read must move to derive
// START: stamping a completion value the passes never saw is fail-OPEN.
//
// This table is provenance, not graph: the write deliberately does NOT advance
// either counter and does NOT invalidate the analysis generation. A stamp that
// bumped the value it was stamping could never converge.
func (s *Store) StampDeriveState(completions []graph.DeriveCompletion, derivedAt int64) error {
	if len(completions) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return err
	}
	for _, c := range completions {
		if c.RepoPrefix == "" {
			continue
		}
		// COALESCE covers a repo with no anchor row yet -- possible when a
		// derive runs before any batch has committed for it. Zero is the
		// honest answer there: nothing has been recorded, and a later real
		// mutation moves the anchor to 1 and reports partial.
		if _, err := tx.Exec(`
INSERT INTO derive_state
  (repo_prefix, derived_gen, derived_content_gen, derived_sha, derived_at,
   pass_version, config_hash, scoped, legacy)
VALUES (
  ?,
  COALESCE((SELECT gen         FROM repo_graph_gen WHERE repo_prefix = ?), 0),
  COALESCE((SELECT content_gen FROM repo_graph_gen WHERE repo_prefix = ?), 0),
  ?, ?, ?, ?, ?, 0)
ON CONFLICT(repo_prefix) DO UPDATE SET
  derived_gen         = excluded.derived_gen,
  derived_content_gen = excluded.derived_content_gen,
  derived_sha         = excluded.derived_sha,
  derived_at          = excluded.derived_at,
  pass_version        = excluded.pass_version,
  config_hash         = excluded.config_hash,
  scoped              = excluded.scoped,
  legacy              = 0`,
			c.RepoPrefix, c.RepoPrefix, c.RepoPrefix, c.DerivedSHA, derivedAt,
			c.PassVersion, c.ConfigHash, boolToInt(c.Scoped)); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// GetDeriveState returns the recorded derived-pass marker for a repo. The bool
// is false when no row exists -- a repo tracked since this table landed that
// has genuinely never been derived. That is a real and reportable state, not a
// missing feature: a repo tracked during daemon warmup is never derived at
// all, permanently, and until this row existed nothing could say so.
func (s *Store) GetDeriveState(repoPrefix string) (graph.DeriveState, bool, error) {
	row := s.db.QueryRow(`
SELECT derived_gen, derived_content_gen, derived_sha, derived_at,
       pass_version, config_hash, scoped, legacy
  FROM derive_state WHERE repo_prefix = ?`, repoPrefix)
	st := graph.DeriveState{RepoPrefix: repoPrefix}
	var scoped, legacy int
	err := row.Scan(&st.DerivedGen, &st.DerivedContentGen, &st.DerivedSHA, &st.DerivedAt,
		&st.PassVersion, &st.ConfigHash, &scoped, &legacy)
	if err == sql.ErrNoRows {
		return graph.DeriveState{RepoPrefix: repoPrefix}, false, nil
	}
	if err != nil {
		return graph.DeriveState{RepoPrefix: repoPrefix}, false, err
	}
	st.Scoped = scoped != 0
	st.Legacy = legacy != 0
	return st, true, nil
}

// GetRepoGraphGen returns a repo's two counters: gen (any graph mutation) and
// content_gen (only the indexer parsing or dropping a file). Zero with a false
// bool means no row: nothing has ever committed a change for that prefix.
//
// Readiness compares stage stamps against contentGen. gen is provenance.
func (s *Store) GetRepoGraphGen(repoPrefix string) (gen, contentGen int64, found bool, err error) {
	err = s.db.QueryRow(
		`SELECT gen, content_gen FROM repo_graph_gen WHERE repo_prefix = ?`, repoPrefix,
	).Scan(&gen, &contentGen)
	if err == sql.ErrNoRows {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return gen, contentGen, true, nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RefreshDeriveState advances the recorded generation for repos that ALREADY
// hold a real completion, and creates nothing. It returns how many rows moved.
//
// This is the incremental derive's write, and the distinction from
// StampDeriveState is the whole reason it exists. The incremental passes run
// only the derived families one edit invalidated — they never run implements
// inference, framework synthesis or cross-repo detection over the whole repo.
// So they can renew a completion the global passes established, but they
// cannot establish one. A repo tracked during daemon warmup is never globally
// derived at all; if a single saved file could stamp it, the readiness column
// would report "ready" for the exact repo whose queries silently return a
// subset, which is worse than having no column.
//
// Zero rows affected is the correct and expected outcome for such a repo: it
// keeps reading "never derived" until a global derive actually runs. The
// legacy = 0 guard says the same thing about a pre-v13 row, whose true state
// is unknowable and must not be laundered into currency by an unrelated save.
// pass_version and config_hash are deliberately left alone — they describe
// what produced the completion, and this pass did not produce it.
func (s *Store) RefreshDeriveState(prefixes []string, derivedAt int64) (int, error) {
	if len(prefixes) == 0 {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.beginWrite()
	if err != nil {
		return 0, err
	}
	refreshed := 0
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		res, err := tx.Exec(`
UPDATE derive_state
   SET derived_gen         = COALESCE((SELECT gen         FROM repo_graph_gen WHERE repo_prefix = ?), 0),
       derived_content_gen = COALESCE((SELECT content_gen FROM repo_graph_gen WHERE repo_prefix = ?), 0),
       derived_at          = ?
 WHERE repo_prefix = ? AND legacy = 0`, prefix, prefix, derivedAt, prefix)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		if n, err := res.RowsAffected(); err == nil {
			refreshed += int(n)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return refreshed, nil
}
