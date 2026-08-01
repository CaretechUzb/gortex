package store_sqlite

import "github.com/zzet/gortex/internal/graph"

// ScanRepoCapabilityEdges reads only the source repository and logical edge
// identity needed by capability synthesis. The id keyset freezes the generation
// and bounds every allocation; each cursor is closed before yield runs so the
// callback may safely re-enter the store.
func (s *Store) ScanRepoCapabilityEdges(
	repoPrefixes []string,
	pageSize int,
	yield func([]graph.RepoCapabilityEdge) bool,
) {
	reposJSON, ok := projectionJSON(repoPrefixes)
	if !ok || yield == nil {
		return
	}
	if pageSize <= 0 {
		pageSize = 256
	}

	var highWater int64
	if err := s.db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM edges`).Scan(&highWater); err != nil {
		panicOnFatal(err)
		return
	}
	if highWater == 0 {
		return
	}

	const query = `
WITH requested_repos(repo_prefix) AS (
    SELECT CAST(value AS TEXT) FROM json_each(?)
)
SELECT e.id, n.repo_prefix,
       e.from_id, e.to_id, e.kind, e.file_path, e.line
FROM edges AS e NOT INDEXED
JOIN nodes AS n ON n.id = e.from_id
JOIN requested_repos AS r ON r.repo_prefix = n.repo_prefix
LEFT JOIN nodes AS target ON target.id = e.to_id
WHERE e.id > ? AND e.id <= ?
  AND e.kind IN (?, ?, ?, ?)
  AND (e.kind NOT IN (?, ?) OR target.kind = ?)
ORDER BY e.id
LIMIT ?`

	lastID := int64(0)
	for lastID < highWater {
		rows, err := s.db.Query(
			query,
			reposJSON, lastID, highWater,
			string(graph.EdgeReadsConfig), string(graph.EdgeReads),
			string(graph.EdgeWrites), string(graph.EdgeCalls),
			string(graph.EdgeReads), string(graph.EdgeWrites), string(graph.KindField),
			pageSize,
		)
		if err != nil {
			panicOnFatal(err)
			return
		}

		page := make([]graph.RepoCapabilityEdge, 0, pageSize)
		for rows.Next() {
			var (
				edgeID int64
				row    graph.RepoCapabilityEdge
			)
			if err := rows.Scan(
				&edgeID,
				&row.RepoPrefix,
				&row.Identity.From,
				&row.Identity.To,
				&row.Identity.Kind,
				&row.Identity.FilePath,
				&row.Identity.Line,
			); err != nil {
				_ = rows.Close()
				panicOnFatal(err)
				return
			}
			lastID = edgeID
			page = append(page, row)
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil {
			panicOnFatal(rowsErr)
			return
		}
		if closeErr != nil {
			panicOnFatal(closeErr)
			return
		}
		if len(page) == 0 {
			return
		}
		if !yield(page) {
			return
		}
		if len(page) < pageSize {
			return
		}
	}
}

var _ graph.RepoCapabilityEdgeScanner = (*Store)(nil)
