package store_sqlite

import "github.com/zzet/gortex/internal/graph"

const (
	exactEdgeIdentityParamsPerRow = 5
	// The runtime variable limit is the primary bound. This additional row cap
	// prevents an unusually permissive SQLite build from retaining an
	// unbounded VALUES relation in the driver.
	exactEdgeIdentityMaxRows = 2048
)

// sqliteEdgeIdentityLookupStats is package-private instrumentation for tests.
// Statements counts actual query attempts (including a failed oversized
// prepare); Retries makes adaptive variable-limit fallback observable.
type sqliteEdgeIdentityLookupStats struct {
	Statements    int
	Retries       int
	MaxKeys       int
	MaxBoundBytes int
}

var _ graph.EdgeIdentityBatchFinder = (*Store)(nil)

// FindEdgesByIdentities fetches current edges by the complete five-column
// logical key. The projection is optional on graph.Store so other backends
// remain compatible; resolver liveness uses it when present and retains its
// set-oriented candidate fallback otherwise.
func (s *Store) FindEdgesByIdentities(identities []graph.EdgeIdentity) map[graph.EdgeIdentity]*graph.Edge {
	edges, _ := s.findEdgesByIdentities(identities)
	return edges
}

func (s *Store) findEdgesByIdentities(identities []graph.EdgeIdentity) (map[graph.EdgeIdentity]*graph.Edge, sqliteEdgeIdentityLookupStats) {
	found := make(map[graph.EdgeIdentity]*graph.Edge)
	var stats sqliteEdgeIdentityLookupStats
	if len(identities) == 0 {
		return found, stats
	}

	// Deduplicate before binding so repeated resolver work neither consumes
	// SQLite variables nor inflates the bounded-query count.
	unique := make([]graph.EdgeIdentity, 0, len(identities))
	seen := make(map[graph.EdgeIdentity]struct{}, len(identities))
	for _, identity := range identities {
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		unique = append(unique, identity)
	}

	variableLimit := s.edgeIdentityVariableLimit()
	for start := 0; start < len(unique); {
		maxRows := batchRowsForVariableLimit(variableLimit, exactEdgeIdentityParamsPerRow, exactEdgeIdentityMaxRows)
		if remaining := len(unique) - start; maxRows > remaining {
			maxRows = remaining
		}

		args := make([]any, 0, maxRows*exactEdgeIdentityParamsPerRow)
		boundBytes := 0
		keyCount := 0
		for keyCount < maxRows {
			identity := unique[start+keyCount]
			rowArgs := []any{identity.From, identity.To, identity.Kind, identity.FilePath, identity.Line}
			rowBytes := sqliteBoundArgsBytes(rowArgs)
			if keyCount > 0 && boundBytes+rowBytes > sqliteBatchMaxBoundBytes {
				break
			}
			args = append(args, rowArgs...)
			boundBytes += rowBytes
			keyCount++
		}

		query := exactEdgeIdentityQuery(keyCount)

		stats.Statements++
		if keyCount > stats.MaxKeys {
			stats.MaxKeys = keyCount
		}
		if boundBytes > stats.MaxBoundBytes {
			stats.MaxBoundBytes = boundBytes
		}

		rows, err := s.db.Query(query, args...)
		if tooManySQLVariables(err) && keyCount > 1 {
			stats.Retries++
			lowerBatchVariableLimit(&variableLimit, exactEdgeIdentityParamsPerRow, keyCount)
			s.rememberEdgeIdentityVariableLimit(variableLimit)
			continue
		}
		if err != nil {
			panicOnFatal(err)
			return found, stats
		}

		for rows.Next() {
			edge, scanErr := scanEdgeCursor(rows)
			if scanErr != nil {
				_ = rows.Close()
				panicOnFatal(scanErr)
				return found, stats
			}
			if edge == nil {
				continue
			}
			found[graph.EdgeIdentityFor(edge)] = edge
		}
		rowsErr := rows.Err()
		_ = rows.Close()
		if rowsErr != nil {
			panicOnFatal(rowsErr)
			return found, stats
		}
		start += keyCount
	}
	return found, stats
}

func exactEdgeIdentityQuery(rows int) string {
	return `WITH wanted(from_id, to_id, kind, file_path, line) AS (VALUES ` +
		multiValues(rows, exactEdgeIdentityParamsPerRow) + `)
SELECT ` + lookupQualifiedEdgeCols + `
FROM wanted AS w
JOIN edges AS e
  ON e.from_id = w.from_id
 AND e.to_id = w.to_id
 AND e.kind = w.kind
 AND e.file_path = w.file_path
 AND e.line = w.line`
}

func (s *Store) edgeIdentityVariableLimit() int {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.sqliteBatchVariableLimitLocked()
}

func (s *Store) rememberEdgeIdentityVariableLimit(limit int) {
	if limit < 1 {
		return
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.batchVariableLimit == 0 || limit < s.batchVariableLimit {
		s.batchVariableLimit = limit
	}
}
