package store_sqlite

import (
	"database/sql"

	"github.com/zzet/gortex/internal/graph"
)

const defaultTestProjectionPageSize = scopedProjectionPage

func boundedTestProjectionPageSize(pageSize int) int {
	if pageSize <= 0 || pageSize > scopedProjectionPage {
		return defaultTestProjectionPageSize
	}
	return pageSize
}

var _ graph.TestProjectionScanner = (*Store)(nil)

// ScanTestNodeProjections pages metadata-free node rows by the WITHOUT ROWID
// primary key. For a fixed kind, nodes_by_kind implicitly carries the primary
// key and supports this keyset order without decoding or copying Meta.
func (s *Store) ScanTestNodeProjections(kinds []graph.NodeKind, pageSize int, yield func([]graph.TestNodeProjection) bool) {
	if yield == nil {
		return
	}
	pageSize = boundedTestProjectionPageSize(pageSize)
	seen := make(map[graph.NodeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}

		var highWater sql.NullString
		if err := s.db.QueryRow(`SELECT MAX(id) FROM nodes WHERE kind = ?`, kind).Scan(&highWater); err != nil {
			panicOnFatal(err)
			return
		}
		if !highWater.Valid || highWater.String == "" {
			continue
		}

		after := ""
		for after < highWater.String {
			rows, err := s.db.Query(`SELECT id, kind, name, file_path, language
FROM nodes
WHERE kind = ? AND id > ? AND id <= ?
ORDER BY id
LIMIT ?`, kind, after, highWater.String, pageSize)
			if err != nil {
				panicOnFatal(err)
				return
			}
			page := make([]graph.TestNodeProjection, 0, pageSize)
			last := after
			for rows.Next() {
				var row graph.TestNodeProjection
				if err := rows.Scan(&row.ID, &row.Kind, &row.Name, &row.FilePath, &row.Language); err != nil {
					_ = rows.Close()
					panicOnFatal(err)
					return
				}
				last = row.ID
				page = append(page, row)
			}
			rowsErr := rows.Err()
			_ = rows.Close()
			if rowsErr != nil {
				panicOnFatal(rowsErr)
				return
			}
			if len(page) == 0 {
				break
			}
			// The cursor is closed before yielding, so the callback may fetch
			// full nodes or write metadata without database/sql re-entry stalls.
			if !yield(page) {
				return
			}
			if last <= after {
				break
			}
			after = last
		}
	}
}

// ScanTestEdgeProjections pages only the endpoint/location columns consumed by
// annotation classification and EdgeTests synthesis. The frozen integer
// high-water mark excludes rows inserted by a callback from the current pass.
func (s *Store) ScanTestEdgeProjections(kinds []graph.EdgeKind, pageSize int, yield func([]graph.TestEdgeProjection) bool) {
	if yield == nil {
		return
	}
	pageSize = boundedTestProjectionPageSize(pageSize)
	seen := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, duplicate := seen[kind]; duplicate {
			continue
		}
		seen[kind] = struct{}{}

		var highWater sql.NullInt64
		if err := s.db.QueryRow(`SELECT MAX(id) FROM edges WHERE kind = ?`, kind).Scan(&highWater); err != nil {
			panicOnFatal(err)
			return
		}
		if !highWater.Valid || highWater.Int64 <= 0 {
			continue
		}

		var after int64
		for after < highWater.Int64 {
			rows, err := s.db.Query(`SELECT id, from_id, to_id, kind, file_path, line
FROM edges
WHERE kind = ? AND id > ? AND id <= ?
ORDER BY id
LIMIT ?`, kind, after, highWater.Int64, pageSize)
			if err != nil {
				panicOnFatal(err)
				return
			}
			page := make([]graph.TestEdgeProjection, 0, pageSize)
			last := after
			for rows.Next() {
				var row graph.TestEdgeProjection
				if err := rows.Scan(&last, &row.From, &row.To, &row.Kind, &row.FilePath, &row.Line); err != nil {
					_ = rows.Close()
					panicOnFatal(err)
					return
				}
				page = append(page, row)
			}
			rowsErr := rows.Err()
			_ = rows.Close()
			if rowsErr != nil {
				panicOnFatal(rowsErr)
				return
			}
			if len(page) == 0 {
				break
			}
			if !yield(page) {
				return
			}
			if last <= after {
				break
			}
			after = last
		}
	}
}
