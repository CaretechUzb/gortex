package store_sqlite

import (
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

// StampDeriveState records that the global derived passes completed for each
// named repo, against that repo's graph generation as it stands right now.
//
// The generation is read here, in the same transaction as the write, and is
// never accepted from the caller. That is the whole design:
//
//	derive reads the graph at gen 41
//	derive's own passes emit edges          -> repo_graph_gen.gen = 44
//	derive completes, stamps                -> derive_state.derived_gen = 44
//	someone saves a file, batch commits     -> repo_graph_gen.gen = 45
//	readiness: 44 < 45                      -> PARTIAL
//
// Stamping the generation observed at derive START instead -- 41 above -- would
// leave every repo permanently behind after its very first derive, because the
// passes themselves are graph writers and advance the anchor as they run. No
// later derive would repair it: a second derive over unchanged content inserts
// no new edges, so it moves nothing and the gap never closes.
//
// Reading at completion is exact rather than a concession. A derive holds the
// batch-mutation write gate for its entire run, so no content write can
// interleave; the generations between start and completion are its own, and
// the content it read at the start is the content it stamps against here.
//
// This table is provenance, not graph: the write deliberately does NOT advance
// any anchor and does NOT invalidate the analysis generation. A stamp that
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
  (repo_prefix, derived_gen, derived_sha, derived_at, pass_version, config_hash, scoped, legacy)
VALUES (
  ?,
  COALESCE((SELECT gen FROM repo_graph_gen WHERE repo_prefix = ?), 0),
  ?, ?, ?, ?, ?, 0)
ON CONFLICT(repo_prefix) DO UPDATE SET
  derived_gen  = excluded.derived_gen,
  derived_sha  = excluded.derived_sha,
  derived_at   = excluded.derived_at,
  pass_version = excluded.pass_version,
  config_hash  = excluded.config_hash,
  scoped       = excluded.scoped,
  legacy       = 0`,
			c.RepoPrefix, c.RepoPrefix, c.DerivedSHA, derivedAt,
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
SELECT derived_gen, derived_sha, derived_at, pass_version, config_hash, scoped, legacy
  FROM derive_state WHERE repo_prefix = ?`, repoPrefix)
	st := graph.DeriveState{RepoPrefix: repoPrefix}
	var scoped, legacy int
	err := row.Scan(&st.DerivedGen, &st.DerivedSHA, &st.DerivedAt,
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

// GetRepoGraphGen returns a repo's current mutation anchor. Zero with a false
// bool means no row: nothing has ever committed a change for that prefix.
func (s *Store) GetRepoGraphGen(repoPrefix string) (int64, bool, error) {
	var gen int64
	err := s.db.QueryRow(`SELECT gen FROM repo_graph_gen WHERE repo_prefix = ?`, repoPrefix).Scan(&gen)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return gen, true, nil
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
   SET derived_gen = COALESCE((SELECT gen FROM repo_graph_gen WHERE repo_prefix = ?), 0),
       derived_at  = ?
 WHERE repo_prefix = ? AND legacy = 0`, prefix, derivedAt, prefix)
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
