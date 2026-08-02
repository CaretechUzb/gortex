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
	reindexKeyParamsPerRow                = 5
	reindexRowParamsPerRow                = edgeInsertParams
	reindexRowMaxChunkSize                = edgeInsertMaxChunkSize
	reindexResolvedUpdateParamsPerRow     = 15
	reindexResolvedKindUpdateParamsPerRow = 16
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
	oldKey             sqliteReindexKey
	newRow             sqliteReindexRow
	resolvedConversion bool
}

type sqliteReindexSetStats struct {
	selectStatements int
	updateStatements int
	deleteStatements int
	insertStatements int
	updatedRows      int
	deletedRows      int
	insertedRows     int
}

func (s sqliteReindexSetStats) writeStatements() int {
	return s.updateStatements + s.deleteStatements + s.insertStatements
}

func (s *sqliteReindexSetStats) add(other sqliteReindexSetStats) {
	s.selectStatements += other.selectStatements
	s.updateStatements += other.updateStatements
	s.deleteStatements += other.deleteStatements
	s.insertStatements += other.insertStatements
	s.updatedRows += other.updatedRows
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

	sameKindUpdates, kindChangingUpdates, deletes, inserts, resolvedConversionFastPath :=
		sqliteResolvedConversionUpdatePlan(mutations)
	if resolvedConversionFastPath {
		updated, statements, repairDeletes, repairInserts, updateErr :=
			updateSQLiteResolvedConversionsTxLimited(tx, sameKindUpdates, false, &variableLimit)
		if updateErr != nil {
			return stats, false, false, nil, updateErr
		}
		stats.updatedRows += updated
		stats.updateStatements += statements
		deletes = append(deletes, repairDeletes...)
		inserts = append(inserts, repairInserts...)

		updated, statements, repairDeletes, repairInserts, updateErr =
			updateSQLiteResolvedConversionsTxLimited(tx, kindChangingUpdates, true, &variableLimit)
		if updateErr != nil {
			return stats, false, false, nil, updateErr
		}
		stats.updatedRows += updated
		stats.updateStatements += statements
		deletes = append(deletes, repairDeletes...)
		inserts = append(inserts, repairInserts...)
	} else {
		initial, selectStatements, selectErr := sqliteReindexRowsTxLimited(tx, keys, &variableLimit)
		if selectErr != nil {
			return stats, false, false, nil, selectErr
		}
		stats.selectStatements = selectStatements
		deletes, inserts = simulateSQLiteReindexSet(initial, keys, mutations)
	}

	stats.deletedRows, stats.deleteStatements, err = deleteSQLiteReindexRowsTxLimited(tx, deletes, &variableLimit)
	if err != nil {
		return stats, false, false, nil, err
	}
	stats.insertedRows, stats.insertStatements, err = insertSQLiteReindexRowsTxLimited(tx, inserts, &variableLimit)
	if err != nil {
		return stats, false, false, nil, err
	}
	if !resolvedConversionFastPath && stats.insertedRows != len(inserts) {
		return stats, false, false, nil, fmt.Errorf(
			"store_sqlite: set reindex inserted %d of %d simulated rows",
			stats.insertedRows, len(inserts),
		)
	}
	// Every in-place candidate is unique across both its old and new logical
	// key. UPDATE OR IGNORE completes the common case in one write; a short
	// RowsAffected count repairs the whole bounded chunk through the existing
	// ordered delete/insert path. Converging and duplicate-old mutations always
	// take that fallback directly, preserving first-candidate-wins semantics.
	changed = stats.updatedRows > 0 || stats.deletedRows > 0 || stats.insertedRows > 0
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
		mutations = append(mutations, sqliteReindexMutation{
			oldKey: oldKey,
			newRow: newRow,
			resolvedConversion: !reindex.RefreshIdentity &&
				graph.IsUnresolvedTarget(oldKey.toID) &&
				!graph.IsUnresolvedTarget(newRow.key.toID),
		})
		addKey(oldKey)
		addKey(newRow.key)
	}
	return mutations, keys, nil
}

// sqliteResolvedConversionSet recognizes the dominant full-resolve mutation
// shape. Every old identity is in the unresolved target namespace and every
// new identity is outside it, so the old and new key domains are disjoint.
// Ordered set simulation therefore reduces exactly to deleting each old key
// once and INSERT OR IGNORE-ing new rows in input order: existing destinations
// win, and the first of several converging candidates wins. Mixed, refresh, and
// unresolved-destination batches retain the generic prefetch + simulation path.
func sqliteResolvedConversionSet(mutations []sqliteReindexMutation) (
	deletes []sqliteReindexKey,
	inserts []sqliteReindexRow,
	ok bool,
) {
	if len(mutations) == 0 {
		return nil, nil, false
	}
	seenOld := make(map[sqliteReindexKey]struct{}, len(mutations))
	deletes = make([]sqliteReindexKey, 0, len(mutations))
	inserts = make([]sqliteReindexRow, 0, len(mutations))
	for _, mutation := range mutations {
		if !mutation.resolvedConversion {
			return nil, nil, false
		}
		if _, seen := seenOld[mutation.oldKey]; !seen {
			seenOld[mutation.oldKey] = struct{}{}
			deletes = append(deletes, mutation.oldKey)
		}
		// Do not deduplicate destinations: INSERT OR IGNORE preserves the
		// generic simulator's input-order, first-candidate-wins contract.
		inserts = append(inserts, mutation.newRow)
	}
	return deletes, inserts, true
}

// sqliteResolvedConversionUpdatePlan peels off only one-to-one conversions for
// in-place updates. Duplicate old keys and converging destinations retain the
// ordered delete/insert path returned by sqliteResolvedConversionSet.
func sqliteResolvedConversionUpdatePlan(mutations []sqliteReindexMutation) (
	sameKind []sqliteReindexMutation,
	kindChanging []sqliteReindexMutation,
	fallbackDeletes []sqliteReindexKey,
	fallbackInserts []sqliteReindexRow,
	ok bool,
) {
	allDeletes, allInserts, ok := sqliteResolvedConversionSet(mutations)
	if !ok {
		return nil, nil, nil, nil, false
	}

	oldCounts := make(map[sqliteReindexKey]int, len(mutations))
	newCounts := make(map[sqliteReindexKey]int, len(mutations))
	for _, mutation := range mutations {
		oldCounts[mutation.oldKey]++
		newCounts[mutation.newRow.key]++
	}

	fallbackOld := make(map[sqliteReindexKey]struct{}, len(allDeletes))
	fallbackDeletes = make([]sqliteReindexKey, 0, len(allDeletes))
	fallbackInserts = make([]sqliteReindexRow, 0, len(allInserts))
	for _, mutation := range mutations {
		if oldCounts[mutation.oldKey] == 1 && newCounts[mutation.newRow.key] == 1 {
			if mutation.oldKey.kind == mutation.newRow.key.kind {
				sameKind = append(sameKind, mutation)
			} else {
				kindChanging = append(kindChanging, mutation)
			}
			continue
		}
		if _, seen := fallbackOld[mutation.oldKey]; !seen {
			fallbackOld[mutation.oldKey] = struct{}{}
			fallbackDeletes = append(fallbackDeletes, mutation.oldKey)
		}
		fallbackInserts = append(fallbackInserts, mutation.newRow)
	}
	return sameKind, kindChanging, fallbackDeletes, fallbackInserts, true
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

func updateSQLiteResolvedConversionsTxLimited(
	tx *sql.Tx,
	mutations []sqliteReindexMutation,
	updateKind bool,
	variableLimit *int,
) (
	updatedRows int,
	statements int,
	repairDeletes []sqliteReindexKey,
	repairInserts []sqliteReindexRow,
	err error,
) {
	if len(mutations) == 0 {
		return 0, 0, nil, nil, nil
	}
	if variableLimit == nil || *variableLimit <= 0 {
		fallback := sqliteFallbackVariableLimit
		variableLimit = &fallback
	}

	paramsPerRow := reindexResolvedUpdateParamsPerRow
	if updateKind {
		paramsPerRow = reindexResolvedKindUpdateParamsPerRow
	}
	rowLimit := batchRowsForVariableLimit(*variableLimit, paramsPerRow, reindexRowMaxChunkSize)
	for pos := 0; pos < len(mutations); {
		chunkStart := pos
		args := make([]any, 0, rowLimit*paramsPerRow)
		argBytes := 0
		rowCount := 0
		for pos < len(mutations) && rowCount < rowLimit {
			mutation := mutations[pos]
			row := mutation.newRow
			argStart := len(args)
			args = append(args,
				mutation.oldKey.fromID, mutation.oldKey.toID, mutation.oldKey.kind,
				mutation.oldKey.filePath, mutation.oldKey.line, row.key.toID,
			)
			if updateKind {
				args = append(args, row.key.kind)
			}
			args = append(args,
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

		query := sqliteResolvedConversionUpdateStatement(rowCount, updateKind)
		result, execErr := tx.Exec(query, args...)
		if tooManySQLVariables(execErr) && rowCount > 1 {
			rowLimit = lowerBatchVariableLimit(variableLimit, paramsPerRow, rowCount)
			pos = chunkStart
			continue
		}
		if execErr != nil {
			return updatedRows, statements, repairDeletes, repairInserts, execErr
		}
		statements++
		affected64, affectedErr := result.RowsAffected()
		if affectedErr != nil {
			return updatedRows, statements, repairDeletes, repairInserts, affectedErr
		}
		affected := int(affected64)
		if affected < 0 || affected > rowCount {
			return updatedRows, statements, repairDeletes, repairInserts, fmt.Errorf(
				"store_sqlite: resolved conversion update affected %d of %d rows",
				affected, rowCount,
			)
		}
		updatedRows += affected
		if affected != rowCount {
			// UPDATE OR IGNORE cannot identify which rows were skipped without
			// materializing RETURNING rows. Replaying the whole bounded chunk is
			// exact: successful rows already occupy their destination, while
			// missing sources and destination collisions are repaired below.
			for _, mutation := range mutations[chunkStart:pos] {
				repairDeletes = append(repairDeletes, mutation.oldKey)
				repairInserts = append(repairInserts, mutation.newRow)
			}
		}
	}
	return updatedRows, statements, repairDeletes, repairInserts, nil
}

func sqliteResolvedConversionUpdateStatement(rowCount int, updateKind bool) string {
	if updateKind {
		return `WITH patch(
		old_from_id, old_to_id, old_kind, file_path, line, new_to_id, new_kind,
		confidence, confidence_label, origin, tier, cross_repo, meta,
		resolve_terminal, resolve_terminal_reason, semantic_source
	) AS (VALUES ` + multiValues(rowCount, reindexResolvedKindUpdateParamsPerRow) + `)
	UPDATE OR IGNORE edges AS e
	SET to_id = p.new_to_id,
		kind = p.new_kind,
		confidence = p.confidence,
		confidence_label = p.confidence_label,
		origin = p.origin,
		tier = p.tier,
		cross_repo = p.cross_repo,
		meta = p.meta,
		resolve_terminal = p.resolve_terminal,
		resolve_terminal_reason = p.resolve_terminal_reason,
		semantic_source = p.semantic_source
	FROM patch AS p
	WHERE e.from_id = p.old_from_id
		AND e.to_id = p.old_to_id
		AND e.kind = p.old_kind
		AND e.file_path = p.file_path
		AND e.line = p.line`
	}
	return `WITH patch(
		old_from_id, old_to_id, kind, file_path, line, new_to_id,
		confidence, confidence_label, origin, tier, cross_repo, meta,
		resolve_terminal, resolve_terminal_reason, semantic_source
	) AS (VALUES ` + multiValues(rowCount, reindexResolvedUpdateParamsPerRow) + `)
	UPDATE OR IGNORE edges AS e
	SET to_id = p.new_to_id,
		confidence = p.confidence,
		confidence_label = p.confidence_label,
		origin = p.origin,
		tier = p.tier,
		cross_repo = p.cross_repo,
		meta = p.meta,
		resolve_terminal = p.resolve_terminal,
		resolve_terminal_reason = p.resolve_terminal_reason,
		semantic_source = p.semantic_source
	FROM patch AS p
	WHERE e.from_id = p.old_from_id
		AND e.to_id = p.old_to_id
		AND e.kind = p.kind
		AND e.file_path = p.file_path
		AND e.line = p.line`
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
