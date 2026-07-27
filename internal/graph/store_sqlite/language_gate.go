package store_sqlite

import (
	"database/sql"
	"errors"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// HasLanguage reports whether any indexed node carries the given language
// (backs the resolver's graphHasLanguage gate). nodes has no
// language-leading index, so a bare language predicate would scan the
// table exactly on the miss — the case a gate exists for. The recursive
// CTE instead skip-scans the distinct repo prefixes (one MIN seek each,
// empty solo-repo prefix included) and probes each through the
// nodes_by_repo_language_name partial index — the non-empty-name
// predicate is what makes it eligible. Unexpected errors answer true so
// a gated pass runs rather than being silently skipped.
func (s *Store) HasLanguage(lang string) bool {
	if lang == "" {
		return false
	}
	var one int
	err := s.db.QueryRow(`
WITH RECURSIVE repo_prefixes(rp) AS (
    SELECT MIN(repo_prefix) FROM nodes
    UNION ALL
    SELECT (SELECT MIN(repo_prefix) FROM nodes WHERE repo_prefix > rp)
    FROM repo_prefixes WHERE rp IS NOT NULL
)
SELECT 1 FROM repo_prefixes
WHERE rp IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM nodes
      WHERE repo_prefix = rp AND language = ? AND name <> ''
  )
LIMIT 1`, lang).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		panicOnFatal(err)
		return true
	}
	return true
}

// NodesByKindLang is NodesByKind with the language predicate pushed into
// SQL, so only the matching language's rows are decoded and cross the
// boundary. Satisfies the resolver's optional nodesByKindLang interface,
// which previously always fell back to NodesByKind + an in-Go filter.
func (s *Store) NodesByKindLang(kind graph.NodeKind, lang string) iter.Seq[*graph.Node] {
	return func(yield func(*graph.Node) bool) {
		out := s.queryNodesSQL(`SELECT `+lookupNodeCols+` FROM nodes WHERE kind = ? AND language = ?`,
			string(kind), lang)
		for _, n := range out {
			if !yield(n) {
				return
			}
		}
	}
}
