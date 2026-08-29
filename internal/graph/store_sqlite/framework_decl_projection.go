package store_sqlite

import (
	"database/sql"
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// FrameworkDeclNodesSeq projects the six columns a framework declaration
// index reads. lookupNodeCols carries thirty-five, including doc, signature,
// section_text and the search_* projections — none of which a declaration
// index consults, and all of which dominate the decoded bytes on a corpus
// whose nodes carry bodies.
//
// It reuses the paged resolver projection rather than a single cursor: the
// Odoo binders re-enter the store while they build, and a long-lived read
// cursor over the whole nodes table would hold the connection for the
// duration of the build.
func (s *Store) FrameworkDeclNodesSeq(kinds ...graph.NodeKind) iter.Seq[graph.FrameworkDeclNode] {
	return resolverNodeProjectionSeq(
		s, kinds, nil, `id, kind, name, file_path, language, meta`,
		func(rows *sql.Rows) (graph.FrameworkDeclNode, string, error) {
			var row graph.FrameworkDeclNode
			var kind string
			var blob []byte
			if err := rows.Scan(
				&row.ID, &kind, &row.Name, &row.FilePath, &row.Language, &blob,
			); err != nil {
				return row, "", err
			}
			row.Kind = graph.NodeKind(kind)
			if len(blob) > 0 {
				meta, err := decodeMeta(blob)
				if err != nil {
					return row, row.ID, err
				}
				row.Meta = meta
			}
			return row, row.ID, nil
		},
	)
}

// FrameworkDeclIdentitiesSeq projects the three columns an ID-join needs.
// No metadata is decoded and, unlike NodeIDNamesByKindsSeq, no ordering is
// imposed beyond the paged projection's primary-key walk — the consumers
// build unordered maps, and a name-ordered scan of the method table is a
// filesort over the largest kind in the store.
func (s *Store) FrameworkDeclIdentitiesSeq(kinds ...graph.NodeKind) iter.Seq[graph.FrameworkDeclIdentity] {
	return resolverNodeProjectionSeq(
		s, kinds, nil, `id, kind, name`,
		func(rows *sql.Rows) (graph.FrameworkDeclIdentity, string, error) {
			var row graph.FrameworkDeclIdentity
			var kind string
			if err := rows.Scan(&row.ID, &kind, &row.Name); err != nil {
				return row, "", err
			}
			row.Kind = graph.NodeKind(kind)
			return row, row.ID, nil
		},
	)
}
