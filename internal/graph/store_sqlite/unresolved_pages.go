package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

const unresolvedEdgePredicate = `is_unresolved = 1
  AND NOT (to_id >= 'unresolved::fnvalue::' AND to_id < 'unresolved::fnvalue:;')
  AND to_id NOT LIKE '%::unresolved::fnvalue::%'`

var _ graph.UnresolvedEdgePager = (*Store)(nil)

// BeginUnresolvedEdgeScan captures a stable rowid high-water mark. Reindexing
// an edge may delete and insert its row; the replacement receives a larger id
// and therefore cannot be visited twice by the same resolver pass.
//
// PendingBefore is deliberately unknown for a non-empty frontier. Computing
// it with COUNT(*) made every warm scoped resolve walk the entire unresolved
// partial index before the first scoped page could run. The descending id
// probe stops after one indexed entry; the resolver derives the exact
// diagnostic count while consuming pages.
func (s *Store) BeginUnresolvedEdgeScan(ctx context.Context) (graph.UnresolvedEdgeScan, error) {
	scan := graph.UnresolvedEdgeScan{PendingBefore: -1}
	err := s.db.QueryRowContext(ctx, `SELECT id
FROM edges INDEXED BY edges_by_unresolved
WHERE `+unresolvedEdgePredicate+`
  AND view_gen = ?
ORDER BY id DESC
LIMIT 1`, s.viewGen).Scan(&scan.HighWaterID)
	if errors.Is(err, sql.ErrNoRows) {
		scan.PendingBefore = 0
		return scan, nil
	}
	return scan, err
}

// ReadUnresolvedEdgePage returns one row- and byte-bounded keyset page. The
// byte bound is measured from the encoded row plus scalar/string fields before
// Meta is decoded; one individually oversized row is admitted to guarantee
// cursor progress.
func (s *Store) ReadUnresolvedEdgePage(ctx context.Context, scan graph.UnresolvedEdgeScan, afterID int64, maxRows, maxBytes int) (graph.UnresolvedEdgePage, error) {
	if maxRows <= 0 {
		maxRows = 2048
	}
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}
	page := graph.UnresolvedEdgePage{NextID: afterID}
	if afterID >= scan.HighWaterID || scan.HighWaterID == 0 {
		page.Exhausted = true
		return page, nil
	}

	predicate := unresolvedEdgePredicate
	args := []any{afterID, scan.HighWaterID}
	anchorIn := func(column string) (string, []any) {
		if len(scan.ScopeAnchors) == 0 {
			return "", nil
		}
		clause := column + ` IN (`
		anchorArgs := make([]any, 0, len(scan.ScopeAnchors))
		for i, anchor := range scan.ScopeAnchors {
			if i > 0 {
				clause += `,`
			}
			clause += `?`
			anchorArgs = append(anchorArgs, anchor)
		}
		return clause + `)`, anchorArgs
	}
	if scan.SkipTerminal {
		// The durable stamp lives in the promoted resolve_terminal column,
		// so a scoped pass's terminal skip runs at the store instead of
		// loading + decoding the row first. NULL (never-stamped /
		// pre-promotion) always passes. A stamped row anchored to a scope
		// repo in either endpoint stays included, tested on the generated
		// from_repo / to_repo_unresolved columns (SQL mirrors of the Go
		// prefix helpers, parity-asserted); stub-qualified targets read as
		// NULL there and fail open to the exact in-memory rule.
		cond := `(resolve_terminal IS NOT 1 OR to_repo_unresolved IS NULL`
		if fromIn, fromArgs := anchorIn(`from_repo`); fromIn != "" {
			cond += ` OR ` + fromIn
			args = append(args, fromArgs...)
		}
		if toIn, toArgs := anchorIn(`to_repo_unresolved`); toIn != "" {
			cond += ` OR ` + toIn
			args = append(args, toArgs...)
		}
		cond += `)`
		predicate += ` AND ` + cond
	}
	if scan.ScopeFilter && len(scan.ScopeAnchors) > 0 {
		// Scoped-pass pushdown of edgeInResolveScope: keep when the source
		// repo is in scope, the target is bare (''), the target's unresolved
		// repo qualifier is in scope, or the shape is one only Go can parse
		// (NULL — fail open). Rows dropped here are exactly the rows the
		// consumer's filterPendingByScope would drop; the in-memory filter
		// still runs and remains the authority.
		cond := `(to_repo_unresolved IS NULL OR to_repo_unresolved = ''`
		if fromIn, fromArgs := anchorIn(`from_repo`); fromIn != "" {
			cond += ` OR ` + fromIn
			args = append(args, fromArgs...)
		}
		if toIn, toArgs := anchorIn(`to_repo_unresolved`); toIn != "" {
			cond += ` OR ` + toIn
			args = append(args, toArgs...)
		}
		cond += `)`
		predicate += ` AND ` + cond
	}
	// The generation binds last among the predicate's parameters, after every
	// optional anchor clause, so the argument list follows the SQL text.
	predicate += ` AND view_gen = ?`
	args = append(args, s.viewGen, maxRows)
	rows, err := s.db.QueryContext(ctx, `SELECT id, `+lookupEdgeCols+`
FROM edges
WHERE id > ? AND id <= ? AND `+predicate+`
ORDER BY id
LIMIT ?`, args...)
	if err != nil {
		return page, err
	}
	defer rows.Close()

	bytesUsed := 0
	rowsRead := 0
	byteStopped := false
	for rows.Next() {
		id, edge, encodedBytes, scanErr := scanUnresolvedEdge(rows)
		if scanErr != nil {
			return page, scanErr
		}
		rowsRead++
		page.NextID = id
		page.Edges = append(page.Edges, edge)
		bytesUsed += encodedBytes
		if bytesUsed >= maxBytes {
			byteStopped = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return page, err
	}
	page.Exhausted = page.NextID >= scan.HighWaterID || (!byteStopped && rowsRead < maxRows)
	return page, nil
}

func scanUnresolvedEdge(scanner interface{ Scan(...any) error }) (int64, *graph.Edge, int, error) {
	var (
		id        int64
		edge      graph.Edge
		metaBlob  sql.RawBytes
		crossRepo int64
		promoted  promotedEdgeMeta
	)
	if err := scanner.Scan(
		&id, &edge.From, &edge.To, &edge.Kind, &edge.FilePath, &edge.Line,
		&edge.Confidence, &edge.ConfidenceLabel, &edge.Origin, &edge.Tier,
		&crossRepo, &metaBlob, &promoted.resolveTerminal,
		&promoted.resolveTerminalReason, &promoted.semanticSource,
	); err != nil {
		return 0, nil, 0, err
	}
	edge.CrossRepo = crossRepo != 0
	if len(metaBlob) > 0 {
		meta, err := decodeMeta(metaBlob)
		if err != nil {
			return 0, nil, 0, fmt.Errorf("decode unresolved edge %d meta: %w", id, err)
		}
		edge.Meta = meta
	}
	restorePromotedEdgeMeta(&edge, promoted)
	encodedBytes := 192 + len(edge.From) + len(edge.To) + len(edge.Kind) +
		len(edge.FilePath) + len(edge.ConfidenceLabel) + len(edge.Origin) +
		len(edge.Tier) + len(metaBlob)
	return id, &edge, encodedBytes, nil
}
