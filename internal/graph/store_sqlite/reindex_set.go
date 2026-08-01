package store_sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	"github.com/zzet/gortex/internal/graph"
)

const (
	// Reindex transactions are simulated as one ordered set so intermediate
	// writes that cancel do not invalidate analysis or inflate receipts. SQL is
	// issued in VALUES relations bounded by the probed connection variable
	// limit and the shared argument-byte budget.
	reindexKeyParamsPerRow = 5
	reindexRowParamsPerRow = edgeInsertParams
	reindexRowMaxChunkSize = edgeInsertMaxChunkSize
)

type sqliteReindexKey struct {
	fromID   string
	toID     string
	kind     string
	filePath string
	line     int
}

type sqliteReindexRow struct {
	key                   sqliteReindexKey
	confidence            float64
	confidenceLabel       string
	origin                string
	tier                  string
	crossRepo             int64
	meta                  []byte
	resolveTerminal       sql.NullBool
	resolveTerminalReason sql.NullString
	semanticSource        sql.NullString
	receiptEdge           *graph.Edge
}

type sqliteReindexMutation struct {
	oldKey sqliteReindexKey
	newRow sqliteReindexRow
}

type sqliteReindexSetStats struct {
	selectStatements int
	deleteStatements int
	insertStatements int
	deletedRows      int
	insertedRows     int
}

func (s sqliteReindexSetStats) writeStatements() int {
	return s.deleteStatements + s.insertStatements
}

func (s *sqliteReindexSetStats) add(other sqliteReindexSetStats) {
	s.selectStatements += other.selectStatements
	s.deleteStatements += other.deleteStatements
	s.insertStatements += other.insertStatements
	s.deletedRows += other.deletedRows
	s.insertedRows += other.insertedRows
}

func (s *Store) reindexEdgesSetOriented(batch []graph.EdgeReindex) (sqliteReindexSetStats, error) {
	var stats sqliteReindexSetStats
	if len(batch) == 0 {
		return stats, nil
	}

	gateCtx, cancelGate := context.WithTimeout(context.Background(), s.sqliteBusyRetryWindow())
	gateErr := s.writeMu.LockContext(gateCtx)
	cancelGate()
	if gateErr != nil {
		// Wrap the recoverable sentinel so ReindexEdges can tell a contended
		// gate (drop the batch, rebind later) apart from a fatal store error
		// (panic). gateErr stays wrapped too, so callers still see the
		// underlying context.DeadlineExceeded / Canceled.
		return stats, fmt.Errorf("%w: %w", errReindexWriterGateContended, gateErr)
	}
	defer s.writeMu.Unlock()

	for txStart := 0; txStart < len(batch); txStart += reindexChunkSize {
		txEnd := minInt(txStart+reindexChunkSize, len(batch))
		var (
			txStats             sqliteReindexSetStats
			changed             bool
			invalidatedAnalysis bool
			receipt             *sqliteReindexReceipt
		)
		err := s.withSQLiteBusyRetry(context.Background(), "reindex_edges", func(ctx context.Context) error {
			var txErr error
			txStats, changed, invalidatedAnalysis, receipt, txErr = s.reindexEdgesSetTransactionLocked(ctx, batch[txStart:txEnd])
			return txErr
		})
		if err != nil {
			return stats, err
		}
		stats.add(txStats)
		if invalidatedAnalysis {
			s.analysisGenerationPresent = false
		}
		s.finishAnalysisMutationLocked(changed)
		if changed {
			s.publishSQLiteReindexReceiptLocked(receipt)
		}
	}
	return stats, nil
}

func (s *Store) reindexEdgesSetTransactionLocked(ctx context.Context, batch []graph.EdgeReindex) (
	stats sqliteReindexSetStats,
	changed bool,
	invalidatedAnalysis bool,
	receipt *sqliteReindexReceipt,
	err error,
) {
	mutations, keys, err := sqliteReindexMutations(batch)
	if err != nil || len(mutations) == 0 {
		return stats, false, false, nil, err
	}

	// Probe before beginWriteContext checks out the writer connection. A fresh
	// single-connection Store cannot discover its limit while its own
	// transaction is holding that connection.
	variableLimit := s.sqliteBatchVariableLimitLocked()
	defer func() {
		// Persist a connection-specific fallback discovered while preparing any
		// of the three bounded reindex statement shapes.
		s.batchVariableLimit = variableLimit
	}()

	tx, err := s.beginWriteContext(ctx)
	if err != nil {
		return stats, false, false, nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	receipt = s.prepareSQLiteReindexReceiptTx(tx, batch)

	initial, selectStatements, err := sqliteReindexRowsTxLimited(tx, keys, &variableLimit)
	if err != nil {
		return stats, false, false, nil, err
	}
	stats.selectStatements = selectStatements
	deletes, inserts := simulateSQLiteReindexSet(initial, keys, mutations)

	stats.deletedRows, stats.deleteStatements, err = deleteSQLiteReindexRowsTxLimited(tx, deletes, &variableLimit)
	if err != nil {
		return stats, false, false, nil, err
	}
	stats.insertedRows, stats.insertStatements, err = insertSQLiteReindexRowsTxLimited(tx, inserts, &variableLimit)
	if err != nil {
		return stats, false, false, nil, err
	}
	if stats.insertedRows != len(inserts) {
		return stats, false, false, nil, fmt.Errorf(
			"store_sqlite: set reindex inserted %d of %d simulated rows",
			stats.insertedRows, len(inserts),
		)
	}
	changed = stats.deletedRows > 0 || stats.insertedRows > 0
	if changed && s.analysisGenerationPresent {
		if err := invalidateAnalysisGenerationTx(tx); err != nil {
			return stats, false, false, nil, err
		}
		invalidatedAnalysis = true
	}
	for _, row := range inserts {
		receipt.recordInserted(row.receiptEdge, true)
	}
	if err := tx.Commit(); err != nil {
		return stats, false, false, nil, err
	}
	committed = true
	return stats, changed, invalidatedAnalysis, receipt, nil
}

func sqliteReindexMutations(batch []graph.EdgeReindex) ([]sqliteReindexMutation, []sqliteReindexKey, error) {
	mutations := make([]sqliteReindexMutation, 0, len(batch))
	keys := make([]sqliteReindexKey, 0, len(batch)*2)
	seen := make(map[sqliteReindexKey]struct{}, len(batch)*2)
	addKey := func(key sqliteReindexKey) {
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}

	for _, reindex := range batch {
		edge := reindex.Edge
		if edge == nil {
			continue
		}
		oldFrom := edge.From
		oldKind := reindex.OldKind
		if oldKind == "" {
			oldKind = edge.Kind
		}
		oldFilePath, oldLine := edge.FilePath, edge.Line
		if reindex.RefreshIdentity {
			if reindex.OldFrom != "" {
				oldFrom = reindex.OldFrom
			}
			oldFilePath, oldLine = reindex.OldFilePath, reindex.OldLine
		} else if reindex.OldTo == edge.To && oldFrom == edge.From && oldKind == edge.Kind {
			continue
		}
		newRow, err := sqliteReindexRowForEdge(edge)
		if err != nil {
			return nil, nil, err
		}
		oldKey := sqliteReindexKey{
			fromID: oldFrom, toID: reindex.OldTo, kind: string(oldKind),
			filePath: oldFilePath, line: oldLine,
		}
		mutations = append(mutations, sqliteReindexMutation{oldKey: oldKey, newRow: newRow})
		addKey(oldKey)
		addKey(newRow.key)
	}
	return mutations, keys, nil
}

func sqliteReindexRowForEdge(edge *graph.Edge) (sqliteReindexRow, error) {
	promoted, blobMeta := extractPromotedEdgeMeta(edge.Meta)
	meta, err := encodeMeta(blobMeta)
	if err != nil {
		return sqliteReindexRow{}, err
	}
	var crossRepo int64
	if edge.CrossRepo {
		crossRepo = 1
	}
	return sqliteReindexRow{
		key: sqliteReindexKey{
			fromID: edge.From, toID: edge.To, kind: string(edge.Kind),
			filePath: edge.FilePath, line: edge.Line,
		},
		confidence:            edge.Confidence,
		confidenceLabel:       edge.ConfidenceLabel,
		origin:                edge.Origin,
		tier:                  edge.Tier,
		crossRepo:             crossRepo,
		meta:                  meta,
		resolveTerminal:       promoted.resolveTerminal,
		resolveTerminalReason: promoted.resolveTerminalReason,
		semanticSource:        promoted.semanticSource,
		receiptEdge:           edge,
	}, nil
}

func sqliteReindexRowsTxLimited(tx *sql.Tx, keys []sqliteReindexKey, variableLimit *int) (map[sqliteReindexKey]sqliteReindexRow, int, error) {
	out := make(map[sqliteReindexKey]sqliteReindexRow, len(keys))
	if len(keys) == 0 {
		return out, 0, nil
	}
	if variableLimit == nil || *variableLimit <= 0 {
		fallback := sqliteFallbackVariableLimit
		variableLimit = &fallback
	}

	rowLimit := batchRowsForVariableLimit(*variableLimit, reindexKeyParamsPerRow, len(keys))
	statements := 0
	for pos := 0; pos < len(keys); {
		chunkStart := pos
		args := make([]any, 0, rowLimit*reindexKeyParamsPerRow)
		argBytes := 0
		rowCount := 0
		for pos < len(keys) && rowCount < rowLimit {
			key := keys[pos]
			argStart := len(args)
			args = append(args, key.fromID, key.toID, key.kind, key.filePath, key.line)
			rowBytes := sqliteBoundArgsBytes(args[argStart:])
			if rowCount > 0 && argBytes+rowBytes > sqliteBatchMaxBoundBytes {
				args = args[:argStart]
				break
			}
			pos++
			rowCount++
			argBytes += rowBytes
		}

		query := `WITH wanted(from_id, to_id, kind, file_path, line) AS (VALUES ` + multiValues(rowCount, reindexKeyParamsPerRow) + `)
		SELECT e.from_id, e.to_id, e.kind, e.file_path, e.line,
			e.confidence, e.confidence_label, e.origin, e.tier, e.cross_repo,
			e.meta, e.resolve_terminal, e.resolve_terminal_reason, e.semantic_source
		FROM wanted AS w
		JOIN edges AS e
		  ON e.from_id = w.from_id
		 AND e.to_id = w.to_id
		 AND e.kind = w.kind
		 AND e.file_path = w.file_path
		 AND e.line = w.line`
		rows, err := tx.Query(query, args...)
		if tooManySQLVariables(err) && rowCount > 1 {
			rowLimit = lowerBatchVariableLimit(variableLimit, reindexKeyParamsPerRow, rowCount)
			pos = chunkStart
			continue
		}
		if err != nil {
			return nil, statements, err
		}
		statements++
		for rows.Next() {
			var row sqliteReindexRow
			if err := rows.Scan(
				&row.key.fromID, &row.key.toID, &row.key.kind, &row.key.filePath, &row.key.line,
				&row.confidence, &row.confidenceLabel, &row.origin, &row.tier, &row.crossRepo,
				&row.meta, &row.resolveTerminal, &row.resolveTerminalReason, &row.semanticSource,
			); err != nil {
				_ = rows.Close()
				return nil, statements, err
			}
			out[row.key] = row
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, statements, err
		}
		if err := rows.Close(); err != nil {
			return nil, statements, err
		}
	}
	return out, statements, nil
}

func simulateSQLiteReindexSet(
	initial map[sqliteReindexKey]sqliteReindexRow,
	keys []sqliteReindexKey,
	mutations []sqliteReindexMutation,
) (deletes []sqliteReindexKey, inserts []sqliteReindexRow) {
	state := make(map[sqliteReindexKey]sqliteReindexRow, len(initial)+len(mutations))
	for key, row := range initial {
		state[key] = row
	}
	for _, mutation := range mutations {
		delete(state, mutation.oldKey)
		if _, exists := state[mutation.newRow.key]; !exists {
			state[mutation.newRow.key] = mutation.newRow
		}
	}

	for _, key := range keys {
		before, existed := initial[key]
		after, remains := state[key]
		switch {
		case existed && !remains:
			deletes = append(deletes, key)
		case !existed && remains:
			inserts = append(inserts, after)
		case existed && remains && !equalSQLiteReindexRows(before, after):
			deletes = append(deletes, key)
			inserts = append(inserts, after)
		}
	}
	return deletes, inserts
}

func equalSQLiteReindexRows(left, right sqliteReindexRow) bool {
	return left.key == right.key &&
		left.confidence == right.confidence &&
		left.confidenceLabel == right.confidenceLabel &&
		left.origin == right.origin &&
		left.tier == right.tier &&
		left.crossRepo == right.crossRepo &&
		(left.meta == nil) == (right.meta == nil) && bytes.Equal(left.meta, right.meta) &&
		left.resolveTerminal == right.resolveTerminal &&
		left.resolveTerminalReason == right.resolveTerminalReason &&
		left.semanticSource == right.semanticSource
}

func deleteSQLiteReindexRowsTxLimited(tx *sql.Tx, keys []sqliteReindexKey, variableLimit *int) (int, int, error) {
	if len(keys) == 0 {
		return 0, 0, nil
	}
	if variableLimit == nil || *variableLimit <= 0 {
		fallback := sqliteFallbackVariableLimit
		variableLimit = &fallback
	}

	rowLimit := batchRowsForVariableLimit(*variableLimit, reindexKeyParamsPerRow, len(keys))
	changed := 0
	statements := 0
	for pos := 0; pos < len(keys); {
		chunkStart := pos
		args := make([]any, 0, rowLimit*reindexKeyParamsPerRow)
		argBytes := 0
		rowCount := 0
		for pos < len(keys) && rowCount < rowLimit {
			key := keys[pos]
			argStart := len(args)
			args = append(args, key.fromID, key.toID, key.kind, key.filePath, key.line)
			rowBytes := sqliteBoundArgsBytes(args[argStart:])
			if rowCount > 0 && argBytes+rowBytes > sqliteBatchMaxBoundBytes {
				args = args[:argStart]
				break
			}
			pos++
			rowCount++
			argBytes += rowBytes
		}

		query := `WITH doomed(from_id, to_id, kind, file_path, line) AS (VALUES ` + multiValues(rowCount, reindexKeyParamsPerRow) + `)
		DELETE FROM edges
		WHERE id IN (
			SELECT e.id
			FROM edges AS e
			JOIN doomed AS d
			  ON e.from_id = d.from_id
			 AND e.to_id = d.to_id
			 AND e.kind = d.kind
			 AND e.file_path = d.file_path
			 AND e.line = d.line
		)`
		result, err := tx.Exec(query, args...)
		if tooManySQLVariables(err) && rowCount > 1 {
			rowLimit = lowerBatchVariableLimit(variableLimit, reindexKeyParamsPerRow, rowCount)
			pos = chunkStart
			continue
		}
		if err != nil {
			return changed, statements, err
		}
		statements++
		rows, err := result.RowsAffected()
		if err != nil {
			return changed, statements, err
		}
		changed += int(rows)
	}
	return changed, statements, nil
}

func insertSQLiteReindexRowsTxLimited(tx *sql.Tx, rows []sqliteReindexRow, variableLimit *int) (int, int, error) {
	if len(rows) == 0 {
		return 0, 0, nil
	}
	if variableLimit == nil || *variableLimit <= 0 {
		fallback := sqliteFallbackVariableLimit
		variableLimit = &fallback
	}

	rowLimit := batchRowsForVariableLimit(*variableLimit, reindexRowParamsPerRow, reindexRowMaxChunkSize)
	changed := 0
	statements := 0
	for pos := 0; pos < len(rows); {
		chunkStart := pos
		args := make([]any, 0, rowLimit*reindexRowParamsPerRow)
		argBytes := 0
		rowCount := 0
		for pos < len(rows) && rowCount < rowLimit {
			row := rows[pos]
			argStart := len(args)
			args = append(args,
				row.key.fromID, row.key.toID, row.key.kind, row.key.filePath, row.key.line,
				row.confidence, row.confidenceLabel, row.origin, row.tier,
				row.crossRepo, row.meta, row.resolveTerminal, row.resolveTerminalReason, row.semanticSource,
			)
			rowBytes := sqliteBoundArgsBytes(args[argStart:])
			if rowCount > 0 && argBytes+rowBytes > sqliteBatchMaxBoundBytes {
				args = args[:argStart]
				break
			}
			pos++
			rowCount++
			argBytes += rowBytes
		}

		query := `INSERT OR IGNORE INTO edges (` + edgeInsertColumns + `) VALUES ` + multiValues(rowCount, reindexRowParamsPerRow)
		result, err := tx.Exec(query, args...)
		if tooManySQLVariables(err) && rowCount > 1 {
			rowLimit = lowerBatchVariableLimit(variableLimit, reindexRowParamsPerRow, rowCount)
			pos = chunkStart
			continue
		}
		if err != nil {
			return changed, statements, err
		}
		statements++
		inserted, err := result.RowsAffected()
		if err != nil {
			return changed, statements, err
		}
		changed += int(inserted)
	}
	return changed, statements, nil
}
