package store_sqlite

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// DanglingEdgeTargets answers the un-bind half of a scoped framework pass:
// which edge targets under these id prefixes does no node answer to.
//
// The whole query is an anti-join over edges_by_to(to_id, kind), a covering
// index, so it never touches an edge row. That is what makes it affordable to
// run on every incremental pass: measured on a 1.1M-edge repository it reduces
// 27,282 distinct targets to 537 dangling ones in ~0.4s, against ~199s for the
// whole-repository edge collection it replaces.
//
// The kind predicate is written `+e.kind IN (…)`, and the unary plus is
// load-bearing rather than decorative. Given both an equality on kind and a
// range on to_id the planner prefers edges_by_kind and scans whole kind ranges,
// which on this corpus is 48s against 0.39s for the same 537 rows. The plus
// makes kind a non-indexable filter term, leaving the to_id range as the only
// usable constraint — the opposite of the usual advice, and correct here
// because the range is the selective half. INDEXED BY would express the same
// intent and is avoided for the reason recorded on repoEdgesByKindsQuery: it is
// a hard runtime error whenever the named index cannot serve the query, and
// this path panics on query errors.
func (s *Store) DanglingEdgeTargets(idPrefixes []string, kinds []graph.EdgeKind) []string {
	kindValues, ok := scopedKindValues(kinds)
	if !ok || len(kindValues) == 0 || kindValues[0] == "" {
		return nil
	}
	bounds := make([]string, 0, len(idPrefixes)*2)
	for _, prefix := range idPrefixes {
		if prefix == "" {
			continue
		}
		upper, ok := graph.PrefixUpperBound(prefix)
		if !ok {
			// An all-0xFF prefix has no exclusive upper bound. It cannot occur
			// for a repository prefix or a file path, and inventing an open
			// range here would turn one prefix into a whole-table scan.
			continue
		}
		bounds = append(bounds, prefix, upper)
	}
	if len(bounds) == 0 {
		return nil
	}
	kindsJSON, ok := projectionJSON(kindValues)
	if !ok {
		return nil
	}

	seen := make(map[string]struct{})
	var out []string
	for i := 0; i < len(bounds); i += 2 {
		rows, err := s.db.Query(danglingEdgeTargetsQuery(), bounds[i], bounds[i+1], kindsJSON)
		if err != nil {
			panicOnFatal(err)
			return nil
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return out
			}
			if _, dup := seen[id]; dup {
				continue
			}
			seen[id] = struct{}{}
			out = append(out, id)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			panicOnFatal(err)
			return out
		}
		_ = rows.Close()
	}
	sort.Strings(out)
	return out
}

// danglingEdgeTargetsQuery is a pure string builder (no I/O) so a plan-lock
// test can EXPLAIN the exact production SQL.
func danglingEdgeTargetsQuery() string {
	return `
SELECT DISTINCT e.to_id
FROM edges AS e
WHERE e.to_id >= ? AND e.to_id < ?
  AND +e.kind IN (SELECT CAST(value AS TEXT) FROM json_each(?))
  AND NOT EXISTS (SELECT 1 FROM nodes AS n WHERE n.id = e.to_id)`
}

var _ graph.DanglingEdgeTargetReader = (*Store)(nil)
