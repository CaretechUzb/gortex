package store_sqlite

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	modernsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/zzet/gortex/internal/graph"
)

// Keep the legacy conservative statement boundary in collision/order tests;
// production reindexing now derives statement sizes from SQLite's live limit.
const reindexCompatibilityChunkSize = 70

func TestReindexEdgesUsesBoundedSetStatementsAndFirstDuplicateWins(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const (
		fromID = "repo/caller.go::Caller"
		newTo  = "repo/target.go::Target"
	)
	oldEdges := make([]*graph.Edge, 0, reindexCompatibilityChunkSize+1)
	batch := make([]graph.EdgeReindex, 0, reindexCompatibilityChunkSize+1)
	for i := 0; i < reindexCompatibilityChunkSize+1; i++ {
		oldTo := fmt.Sprintf("repo/old-%03d.go::Target", i)
		oldEdges = append(oldEdges, &graph.Edge{
			From: fromID, To: oldTo, Kind: graph.EdgeCalls,
			FilePath: "repo/caller.go", Line: 7, Origin: "old",
		})
		batch = append(batch, graph.EdgeReindex{
			OldTo: oldTo,
			Edge: &graph.Edge{
				From: fromID, To: newTo, Kind: graph.EdgeCalls,
				FilePath: "repo/caller.go", Line: 7,
				Confidence: 0.8, ConfidenceLabel: "confirmed",
				Origin: fmt.Sprintf("candidate-%03d", i), Tier: "semantic",
			},
		})
	}
	store.AddBatch([]*graph.Node{{
		ID: fromID, Kind: graph.KindFunction, Name: "Caller",
		FilePath: "repo/caller.go", RepoPrefix: "repo",
	}}, oldEdges)

	beforeRevision := store.AnalysisMutationRevision()
	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.selectStatements, "all affected identities fit one bounded prefetch")
	assert.Equal(t, 1, stats.deleteStatements)
	assert.Equal(t, 1, stats.insertStatements)
	assert.Equal(t, 2, stats.writeStatements(), "set-oriented writes replace 142 per-edge DELETE/INSERT statements")
	assert.Equal(t, len(batch), stats.deletedRows)
	assert.Equal(t, 1, stats.insertedRows)
	assert.Equal(t, beforeRevision+1, store.AnalysisMutationRevision())

	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	assert.Equal(t, newTo, persisted[0].To)
	assert.Equal(t, "candidate-000", persisted[0].Origin, "INSERT OR IGNORE ordering keeps the first converging payload")
	assert.InDelta(t, 0.8, persisted[0].Confidence, 0.0001)
	assert.Equal(t, "confirmed", persisted[0].ConfidenceLabel)
	assert.Equal(t, "semantic", persisted[0].Tier)

	buildMinimalAnalysisGeneration(t, store, "set-reindex-noop", 0, true)
	beforeRevision = store.AnalysisMutationRevision()
	stats, err = store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.selectStatements)
	assert.Equal(t, 0, stats.writeStatements(), "an idempotent replay must not rewrite rows")
	assert.Equal(t, beforeRevision, store.AnalysisMutationRevision())
	_, found, err := store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	assert.True(t, found, "an idempotent replay must preserve active warm analysis")
}

func TestReindexEdgesRefreshDuplicateUsesLastWriteAndPreservesQuality(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const (
		fromID   = "repo/caller.go::Caller"
		targetID = "repo/target.go::Target"
		filePath = "repo/caller.go"
		line     = 11
	)
	store.AddBatch(nil, []*graph.Edge{{
		From: fromID, To: targetID, Kind: graph.EdgeCalls,
		FilePath: filePath, Line: line,
		Confidence: 0.2, ConfidenceLabel: "heuristic", Origin: "old", Tier: "syntax",
		Meta: map[string]any{"opaque": "old"},
	}})

	first := &graph.Edge{
		From: fromID, To: targetID, Kind: graph.EdgeCalls,
		FilePath: filePath, Line: line,
		Confidence: 0.6, ConfidenceLabel: "candidate", Origin: "first", Tier: "semantic",
		Meta: map[string]any{"opaque": "first"},
	}
	last := &graph.Edge{
		From: fromID, To: targetID, Kind: graph.EdgeCalls,
		FilePath: filePath, Line: line,
		Confidence: 0.91, ConfidenceLabel: "confirmed", Origin: "last", Tier: "compiler",
		CrossRepo: true,
		Meta: map[string]any{
			"opaque":                  "keep",
			"resolve_terminal":        true,
			"resolve_terminal_reason": "resolved",
		},
	}
	batch := []graph.EdgeReindex{
		{Edge: first, OldTo: targetID, RefreshIdentity: true, OldFilePath: filePath, OldLine: line},
		{Edge: last, OldTo: targetID, RefreshIdentity: true, OldFilePath: filePath, OldLine: line},
	}

	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.selectStatements)
	assert.Equal(t, 1, stats.deleteStatements)
	assert.Equal(t, 1, stats.insertStatements)

	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	edge := persisted[0]
	assert.Equal(t, "last", edge.Origin)
	assert.Equal(t, "compiler", edge.Tier)
	assert.InDelta(t, 0.91, edge.Confidence, 0.0001)
	assert.Equal(t, "confirmed", edge.ConfidenceLabel)
	assert.True(t, edge.CrossRepo)
	assert.Equal(t, "keep", edge.Meta["opaque"])
	assert.Equal(t, true, edge.Meta["resolve_terminal"])
	assert.Equal(t, "resolved", edge.Meta["resolve_terminal_reason"])

	beforeRevision := store.AnalysisMutationRevision()
	stats, err = store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.writeStatements(), "transient first-write followed by identical last-write has no final state change")
	assert.Equal(t, beforeRevision, store.AnalysisMutationRevision())
}

func TestReindexEdgesNetCancellationAcrossSQLChunksIsNoop(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	const (
		fromID       = "repo/caller.go::Caller"
		originalTo   = "unresolved::Original"
		intermediate = "unresolved::Intermediate"
		fillerFrom   = "repo/filler.go::Caller"
		fillerTo     = "repo/filler-target.go::Target"
	)
	original := &graph.Edge{
		From: fromID, To: originalTo, Kind: graph.EdgeCalls,
		FilePath: "repo/caller.go", Line: 17,
		Confidence: 0.85, ConfidenceLabel: "confirmed", Origin: "compiler", Tier: "semantic",
		Meta: map[string]any{"opaque": "preserve"},
	}
	filler := &graph.Edge{
		From: fillerFrom, To: fillerTo, Kind: graph.EdgeCalls,
		FilePath: "repo/filler.go", Line: 3, Origin: "filler",
	}
	store.AddBatch(nil, []*graph.Edge{original, filler})
	buildMinimalAnalysisGeneration(t, store, "set-reindex-net-noop", 0, true)
	beforeRevision := store.AnalysisMutationRevision()

	batch := make([]graph.EdgeReindex, 0, reindexCompatibilityChunkSize+1)
	intermediateEdge := *original
	intermediateEdge.To = intermediate
	batch = append(batch, graph.EdgeReindex{Edge: &intermediateEdge, OldTo: originalTo})
	for i := 0; i < reindexCompatibilityChunkSize-1; i++ {
		fillerCandidate := *filler
		batch = append(batch, graph.EdgeReindex{
			Edge:  &fillerCandidate,
			OldTo: fmt.Sprintf("repo/missing-%03d.go::Target", i),
		})
	}
	restored := *original
	batch = append(batch, graph.EdgeReindex{Edge: &restored, OldTo: intermediate})

	token := store.BeginMutationReceipt()
	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	receipt := store.EndMutationReceipt(token)

	assert.Equal(t, 1, stats.selectStatements)
	assert.Equal(t, 0, stats.writeStatements(), "net cancellation must not issue transient writes")
	assert.Equal(t, 0, stats.deletedRows)
	assert.Equal(t, 0, stats.insertedRows)
	assert.Equal(t, beforeRevision, store.AnalysisMutationRevision())
	assert.True(t, receipt.Complete)
	assert.False(t, receipt.ResolutionRelevant, "a net-noop must not create resolver catch-up work")

	_, found, err := store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	assert.True(t, found, "a net-noop must preserve active warm analysis")
	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 1)
	assert.Equal(t, originalTo, persisted[0].To)
	assert.Equal(t, "preserve", persisted[0].Meta["opaque"])
}

func TestReindexEdgesUsesRuntimeVariableBudget(t *testing.T) {
	const edgeCount = 2000
	for _, variableLimit := range []int{sqliteFallbackVariableLimit, sqliteBatchVariableHardCap} {
		t.Run(fmt.Sprintf("limit_%d", variableLimit), func(t *testing.T) {
			store := openReindexReceiptTestStore(t)
			oldEdges := make([]*graph.Edge, 0, edgeCount)
			batch := make([]graph.EdgeReindex, 0, edgeCount)
			for i := 0; i < edgeCount; i++ {
				from := fmt.Sprintf("repo/caller-%04d.go::Caller", i)
				oldTo := fmt.Sprintf("unresolved::Old%04d", i)
				oldEdges = append(oldEdges, &graph.Edge{
					From: from, To: oldTo, Kind: graph.EdgeCalls,
					FilePath: "repo/callers.go", Line: i + 1,
				})
				batch = append(batch, graph.EdgeReindex{
					OldTo: oldTo,
					Edge: &graph.Edge{
						From: from, To: fmt.Sprintf("repo/target-%04d.go::Target", i),
						Kind: graph.EdgeCalls, FilePath: "repo/callers.go", Line: i + 1,
					},
				})
			}
			store.AddBatch(nil, oldEdges)
			store.batchVariableLimit = variableLimit

			stats, err := store.reindexEdgesSetOriented(batch)
			require.NoError(t, err)
			keyRows := batchRowsForVariableLimit(variableLimit, reindexKeyParamsPerRow, edgeCount)
			insertRows := batchRowsForVariableLimit(variableLimit, reindexRowParamsPerRow, reindexRowMaxChunkSize)
			assert.Zero(t, stats.selectStatements, "resolved conversions must not prefetch full edge rows")
			assert.Equal(t, (edgeCount+keyRows-1)/keyRows, stats.deleteStatements)
			assert.Equal(t, (edgeCount+insertRows-1)/insertRows, stats.insertStatements)
			assert.Equal(t, edgeCount, stats.deletedRows)
			assert.Equal(t, edgeCount, stats.insertedRows)
			assert.Equal(t, edgeCount, store.EdgeCount())
		})
	}
}

func TestReindexInsertChunksRespectBoundArgumentBytes(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	tx, err := store.beginWrite()
	require.NoError(t, err)
	largeMeta := bytes.Repeat([]byte("x"), 3<<20)
	rows := make([]sqliteReindexRow, 3)
	for i := range rows {
		rows[i] = sqliteReindexRow{
			key: sqliteReindexKey{
				fromID: fmt.Sprintf("repo/caller-%d.go::Caller", i),
				toID:   fmt.Sprintf("repo/target-%d.go::Target", i),
				kind:   string(graph.EdgeCalls), filePath: "repo/callers.go", line: i + 1,
			},
			meta: largeMeta,
		}
	}
	variableLimit := sqliteBatchVariableHardCap
	inserted, statements, err := insertSQLiteReindexRowsTxLimited(tx, rows, &variableLimit)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	assert.Equal(t, len(rows), inserted)
	assert.Equal(t, len(rows), statements, "each 3 MiB row must stay below the 4 MiB statement budget")
}

func TestReindexEdgesDownshiftsConnectionVariableLimit(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	oldEdges := make([]*graph.Edge, 0, 120)
	batch := make([]graph.EdgeReindex, 0, 120)
	for i := 0; i < 120; i++ {
		from := fmt.Sprintf("repo/caller-%03d.go::Caller", i)
		oldTo := fmt.Sprintf("unresolved::Old%03d", i)
		oldEdges = append(oldEdges, &graph.Edge{
			From: from, To: oldTo, Kind: graph.EdgeCalls,
			FilePath: "repo/callers.go", Line: i + 1,
		})
		batch = append(batch, graph.EdgeReindex{
			OldTo: oldTo,
			Edge: &graph.Edge{
				From: from, To: fmt.Sprintf("repo/target-%03d.go::Target", i),
				Kind: graph.EdgeCalls, FilePath: "repo/callers.go", Line: i + 1,
			},
		})
	}
	store.AddBatch(nil, oldEdges)

	conn, err := store.writerDB.Conn(context.Background())
	require.NoError(t, err)
	const connectionLimit = 220
	_, err = modernsqlite.Limit(conn, int(sqlite3.SQLITE_LIMIT_VARIABLE_NUMBER), connectionLimit)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	store.batchVariableLimit = sqliteBatchVariableHardCap

	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	assert.Equal(t, len(batch), stats.deletedRows)
	assert.Equal(t, len(batch), stats.insertedRows)
	assert.LessOrEqual(t, store.batchVariableLimit, connectionLimit)
	assert.Zero(t, stats.selectStatements, "resolved conversions must not prefetch full edge rows")
	assert.Greater(t, stats.deleteStatements, 1, "the first oversized delete must downshift and retry")
	assert.Equal(t, len(batch), store.EdgeCount())
}

func TestReindexEdgesProbesVariableLimitBeforeWriterCheckout(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	store.writerDB.SetMaxOpenConns(1)
	old := &graph.Edge{
		From: "repo/caller.go::Caller", To: "unresolved::Target", Kind: graph.EdgeCalls,
		FilePath: "repo/caller.go", Line: 7,
	}
	store.AddBatch(nil, []*graph.Edge{old})
	store.batchVariableLimit = 0 // exercise the fresh-store probe path

	done := make(chan error, 1)
	go func() {
		_, err := store.reindexEdgesSetOriented([]graph.EdgeReindex{{
			OldTo: old.To,
			Edge: &graph.Edge{
				From: old.From, To: "repo/target.go::Target", Kind: old.Kind,
				FilePath: old.FilePath, Line: old.Line,
			},
		}})
		done <- err
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		// Let an implementation that checked out the sole connection before
		// probing its limit unwind, so test cleanup cannot remain wedged.
		store.writerDB.SetMaxOpenConns(2)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
		t.Fatal("ReindexEdges waited for its own writer connection while probing SQLite limits")
	}
	assert.Positive(t, store.batchVariableLimit)
}

func TestReindexEdgesResolvedConversionFastPathPreservesSetSemantics(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	const (
		fromID          = "repo/caller.go::Caller"
		filePath        = "repo/caller.go"
		sharedTarget    = "repo/shared.go::Target"
		existingTarget  = "repo/existing.go::Target"
		firstOldTarget  = "unresolved::First"
		secondOldTarget = "unresolved::Second"
		thirdOldTarget  = "unresolved::Third"
	)
	oldEdges := []*graph.Edge{
		{From: fromID, To: firstOldTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 7, Origin: "old-first"},
		{From: fromID, To: secondOldTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 7, Origin: "old-second"},
		{From: fromID, To: thirdOldTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 8, Origin: "old-third"},
		{From: fromID, To: existingTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 8, Origin: "existing"},
	}
	store.AddBatch(nil, oldEdges)
	buildMinimalAnalysisGeneration(t, store, "resolved-conversion-fast-path", 0, true)
	beforeRevision := store.AnalysisMutationRevision()

	batch := []graph.EdgeReindex{
		{OldTo: firstOldTarget, Edge: &graph.Edge{
			From: fromID, To: sharedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 7,
			Confidence: 0.9, ConfidenceLabel: "confirmed", Origin: "first", Tier: "semantic",
			Meta: map[string]any{"winner": "first"},
		}},
		{OldTo: secondOldTarget, Edge: &graph.Edge{
			From: fromID, To: sharedTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 7,
			Confidence: 0.4, ConfidenceLabel: "heuristic", Origin: "second", Tier: "syntax",
			Meta: map[string]any{"winner": "second"},
		}},
		{OldTo: thirdOldTarget, Edge: &graph.Edge{
			From: fromID, To: existingTarget, Kind: graph.EdgeCalls, FilePath: filePath, Line: 8,
			Confidence: 1, ConfidenceLabel: "confirmed", Origin: "replacement", Tier: "compiler",
		}},
	}

	token := store.BeginMutationReceipt()
	stats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	receipt := store.EndMutationReceipt(token)
	assert.Zero(t, stats.selectStatements, "resolved conversions must not prefetch full edge rows")
	assert.Equal(t, 1, stats.deleteStatements)
	assert.Equal(t, 1, stats.insertStatements)
	assert.Equal(t, len(batch), stats.deletedRows)
	assert.Equal(t, 1, stats.insertedRows, "one duplicate and one existing destination must be ignored")
	assert.Equal(t, beforeRevision+1, store.AnalysisMutationRevision())
	assert.True(t, receipt.Complete)
	assert.False(t, receipt.ResolutionRelevant, "resolved destinations cannot create pending resolver work")

	persisted := store.GetOutEdges(fromID)
	require.Len(t, persisted, 2)
	byTarget := make(map[string]*graph.Edge, len(persisted))
	for _, edge := range persisted {
		byTarget[edge.To] = edge
	}
	shared := byTarget[sharedTarget]
	require.NotNil(t, shared)
	assert.Equal(t, "first", shared.Origin, "input order must preserve first-candidate-wins semantics")
	assert.Equal(t, "semantic", shared.Tier)
	assert.Equal(t, "first", shared.Meta["winner"])
	existing := byTarget[existingTarget]
	require.NotNil(t, existing)
	assert.Equal(t, "existing", existing.Origin, "a pre-existing destination must win over a conversion")

	buildMinimalAnalysisGeneration(t, store, "resolved-conversion-replay", 0, true)
	replayRevision := store.AnalysisMutationRevision()
	token = store.BeginMutationReceipt()
	replayStats, err := store.reindexEdgesSetOriented(batch)
	require.NoError(t, err)
	replayReceipt := store.EndMutationReceipt(token)
	assert.Zero(t, replayStats.selectStatements)
	assert.Zero(t, replayStats.deletedRows)
	assert.Zero(t, replayStats.insertedRows)
	assert.Equal(t, replayRevision, store.AnalysisMutationRevision(), "an idempotent replay must not invalidate analysis")
	assert.True(t, replayReceipt.Complete)
	assert.False(t, replayReceipt.ResolutionRelevant)
	_, found, err := store.LoadActiveAnalysisHeader(77)
	require.NoError(t, err)
	assert.True(t, found, "an idempotent replay must preserve active warm analysis")
}

func BenchmarkReindexEdgesResolvedConversions50K(b *testing.B) {
	const edgeCount = 50_000
	b.StopTimer()
	oldEdges := make([]*graph.Edge, 0, edgeCount)
	batch := make([]graph.EdgeReindex, 0, edgeCount)
	for i := 0; i < edgeCount; i++ {
		from := fmt.Sprintf("repo/caller-%05d.go::Caller", i)
		oldTo := fmt.Sprintf("unresolved::Target%05d", i)
		oldEdges = append(oldEdges, &graph.Edge{
			From: from, To: oldTo, Kind: graph.EdgeCalls,
			FilePath: "repo/callers.go", Line: i + 1,
		})
		batch = append(batch, graph.EdgeReindex{
			OldTo: oldTo,
			Edge: &graph.Edge{
				From: from, To: fmt.Sprintf("repo/target-%05d.go::Target", i),
				Kind: graph.EdgeCalls, FilePath: "repo/callers.go", Line: i + 1,
			},
		})
	}
	b.ReportAllocs()
	b.ReportMetric(edgeCount, "edges/op")
	benchDir := b.TempDir()
	for i := 0; i < b.N; i++ {
		store, err := Open(filepath.Join(benchDir, fmt.Sprintf("graph-%d.sqlite", i)))
		if err != nil {
			b.Fatal(err)
		}
		store.AddBatch(nil, oldEdges)
		b.StartTimer()
		stats, err := store.reindexEdgesSetOriented(batch)
		b.StopTimer()
		if err != nil {
			_ = store.Close()
			b.Fatal(err)
		}
		if stats.selectStatements != 0 || stats.deletedRows != edgeCount || stats.insertedRows != edgeCount {
			_ = store.Close()
			b.Fatalf("unexpected fast-path stats: %+v", stats)
		}
		if err := store.Close(); err != nil {
			b.Fatal(err)
		}
	}
}
