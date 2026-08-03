package store_sqlite

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestScanUnresolvedEdgeIdentitiesBatchedIsBoundedFrozenAndReentrant(t *testing.T) {
	store := openUnresolvedTargetTestStore(t)
	edges := []*graph.Edge{
		{From: "repo/a.go::A", To: "unresolved::A", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 1},
		{From: "repo/resolved.go::R", To: "repo/target.go::R", Kind: graph.EdgeCalls, FilePath: "repo/resolved.go", Line: 2},
		{From: "repo/b.go::B", To: "repo::unresolved::B", Kind: graph.EdgeReferences, FilePath: "repo/b.go", Line: 3},
		{From: "repo/c.go::C", To: "unresolved::C", Kind: graph.EdgeCalls, FilePath: "repo/c.go", Line: 4},
		{From: "repo/import.go::I", To: "unresolved::I", Kind: graph.EdgeImports, FilePath: "repo/import.go", Line: 5},
	}
	store.AddBatch(nil, edges)

	var (
		got       []graph.EdgeIdentity
		pageSizes []int
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		store.ScanUnresolvedEdgeIdentitiesBatched(
			[]graph.EdgeKind{graph.EdgeCalls, graph.EdgeReferences},
			2,
			func(page []graph.EdgeIdentity) bool {
				pageSizes = append(pageSizes, len(page))
				got = append(got, page...)
				if len(pageSizes) == 1 {
					mutations := make([]graph.UnresolvedEdgeTargetReindex, 0, len(page))
					for i, identity := range page {
						mutations = append(mutations, graph.UnresolvedEdgeTargetReindex{
							Old: identity, NewTo: fmt.Sprintf("repo/target.go::Resolved%d", i),
						})
					}
					// Both writes must be able to re-enter after the page cursor and
					// active writer gate have been released.
					store.ReindexUnresolvedEdgeTargets(mutations)
					store.AddBatch(nil, []*graph.Edge{{
						From: "repo/late.go::Late", To: "unresolved::Late", Kind: graph.EdgeCalls,
						FilePath: "repo/late.go", Line: 6,
					}})
				}
				return true
			},
		)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("scanner callback could not re-enter the store after its page")
	}

	assert.Equal(t, []int{2, 1}, pageSizes)
	require.Len(t, got, 3)
	assert.Equal(t, "unresolved::A", got[0].To)
	assert.Equal(t, "repo::unresolved::B", got[1].To)
	assert.Equal(t, "unresolved::C", got[2].To)
	for _, identity := range got {
		assert.NotEqual(t, "unresolved::Late", identity.To, "rows above the frozen high-water must wait for the next pass")
	}
}

func TestScanUnresolvedEdgeIdentitiesBatchedStopsWhenYieldReturnsFalse(t *testing.T) {
	store := openUnresolvedTargetTestStore(t)
	store.AddBatch(nil, []*graph.Edge{
		{From: "repo/a.go::A", To: "unresolved::A", Kind: graph.EdgeCalls, FilePath: "repo/a.go", Line: 1},
		{From: "repo/b.go::B", To: "unresolved::B", Kind: graph.EdgeCalls, FilePath: "repo/b.go", Line: 2},
	})

	calls := 0
	store.ScanUnresolvedEdgeIdentitiesBatched(
		[]graph.EdgeKind{graph.EdgeCalls},
		1,
		func(page []graph.EdgeIdentity) bool {
			calls++
			require.Len(t, page, 1)
			return false
		},
	)
	assert.Equal(t, 1, calls)
}

func TestReindexUnresolvedEdgeTargetsPreservesPayloadAndMissingRowSemantics(t *testing.T) {
	store := openUnresolvedTargetTestStore(t)
	const (
		successFrom   = "repo/success.go::Caller"
		successOld    = "unresolved::Success"
		successTarget = "repo/target.go::Success"
		collisionFrom = "repo/collision.go::Caller"
		collisionOld  = "unresolved::Collision"
		collisionTo   = "repo/target.go::Collision"
		missingFrom   = "repo/missing.go::Caller"
		missingOld    = "unresolved::Missing"
		missingTarget = "repo/target.go::Missing"
	)
	store.AddBatch(nil, []*graph.Edge{
		{
			From: successFrom, To: successOld, Kind: graph.EdgeCalls,
			FilePath: "repo/success.go", Line: 11,
			Confidence: 0.91, ConfidenceLabel: "confirmed", Origin: "semantic", Tier: "compiler",
			CrossRepo: true,
			Meta: map[string]any{
				"payload":                 "keep",
				"resolve_terminal":        true,
				"resolve_terminal_reason": "bound",
				"semantic_source":         "lsp",
			},
		},
		{From: collisionFrom, To: collisionOld, Kind: graph.EdgeReferences, FilePath: "repo/collision.go", Line: 17, Origin: "loser"},
		{From: collisionFrom, To: collisionTo, Kind: graph.EdgeReferences, FilePath: "repo/collision.go", Line: 17, Origin: "existing"},
	})
	successOldIdentity := graph.EdgeIdentity{
		From: successFrom, To: successOld, Kind: graph.EdgeCalls, FilePath: "repo/success.go", Line: 11,
	}
	beforeID := unresolvedTargetStoredEdgeID(t, store, successOldIdentity)
	beforeAnalysisRevision := store.AnalysisMutationRevision()
	beforeMutationRevision := store.MutationRevision()
	batch := []graph.UnresolvedEdgeTargetReindex{
		{Old: successOldIdentity, NewTo: successTarget},
		{Old: graph.EdgeIdentity{
			From: collisionFrom, To: collisionOld, Kind: graph.EdgeReferences,
			FilePath: "repo/collision.go", Line: 17,
		}, NewTo: collisionTo},
		{Old: graph.EdgeIdentity{
			From: missingFrom, To: missingOld, Kind: graph.EdgeCalls,
			FilePath: "repo/missing.go", Line: 23,
		}, NewTo: missingTarget},
	}

	token := store.BeginMutationReceipt()
	stats, err := store.reindexUnresolvedEdgeTargetsOriented(batch)
	require.NoError(t, err)
	receipt := store.EndMutationReceipt(token)
	assert.Equal(t, 1, stats.updateStatements)
	assert.Equal(t, 1, stats.updatedRows)
	assert.Equal(t, 1, stats.deleteStatements)
	assert.Equal(t, 1, stats.deletedRows, "only the uniqueness-collision source should be removed")
	assert.Equal(t, 2, stats.writeStatements())
	assert.Equal(t, beforeAnalysisRevision+1, store.AnalysisMutationRevision())
	assert.Equal(t, beforeMutationRevision+1, store.MutationRevision())
	assert.True(t, receipt.Complete)
	assert.False(t, receipt.ResolutionRelevant)

	successIdentity := successOldIdentity
	successIdentity.To = successTarget
	assert.Equal(t, beforeID, unresolvedTargetStoredEdgeID(t, store, successIdentity))
	success := store.GetOutEdges(successFrom)
	require.Len(t, success, 1)
	assert.Equal(t, successTarget, success[0].To)
	assert.Equal(t, 0.91, success[0].Confidence)
	assert.Equal(t, "confirmed", success[0].ConfidenceLabel)
	assert.Equal(t, "semantic", success[0].Origin)
	assert.Equal(t, "compiler", success[0].Tier)
	assert.True(t, success[0].CrossRepo)
	assert.Equal(t, "keep", success[0].Meta["payload"])
	assert.Equal(t, true, success[0].Meta["resolve_terminal"])
	assert.Equal(t, "bound", success[0].Meta["resolve_terminal_reason"])
	assert.Equal(t, "lsp", success[0].Meta["semantic_source"])

	collision := store.GetOutEdges(collisionFrom)
	require.Len(t, collision, 1)
	assert.Equal(t, collisionTo, collision[0].To)
	assert.Equal(t, "existing", collision[0].Origin)
	assert.Empty(t, store.GetOutEdges(missingFrom), "a source removed after the census must not be resurrected")

	replayAnalysisRevision := store.AnalysisMutationRevision()
	replayMutationRevision := store.MutationRevision()
	replay, err := store.reindexUnresolvedEdgeTargetsOriented(batch)
	require.NoError(t, err)
	assert.Zero(t, replay.updatedRows)
	assert.Zero(t, replay.deletedRows)
	assert.Equal(t, replayAnalysisRevision, store.AnalysisMutationRevision())
	assert.Equal(t, replayMutationRevision, store.MutationRevision())
}

func BenchmarkReindexUnresolvedEdgeTargetsPayloadChurn(b *testing.B) {
	const edgeCount = 5000
	identities := make([]graph.EdgeIdentity, 0, edgeCount)
	targets := make([]string, 0, edgeCount)
	for i := 0; i < edgeCount; i++ {
		identities = append(identities, graph.EdgeIdentity{
			From: fmt.Sprintf("repo/caller-%05d.go::Caller", i),
			To:   fmt.Sprintf("unresolved::Target%05d", i), Kind: graph.EdgeCalls,
			FilePath: "repo/callers.go", Line: i + 1,
		})
		targets = append(targets, fmt.Sprintf("repo/target-%05d.go::Target", i))
	}

	b.Run("full_edge_refetch", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(edgeCount, "edges/op")
		for i := 0; i < b.N; i++ {
			store := openUnresolvedTargetBenchmarkStore(b, fmt.Sprintf("full-%d", i), identities)
			b.StartTimer()
			current := store.FindEdgesByIdentities(identities)
			batch := make([]graph.EdgeReindex, 0, len(identities))
			for pos, identity := range identities {
				edge := current[identity]
				if edge == nil {
					b.Fatalf("missing benchmark edge %d", pos)
				}
				oldTo := edge.To
				edge.To = targets[pos]
				batch = append(batch, graph.EdgeReindex{OldTo: oldTo, Edge: edge})
			}
			store.ReindexEdges(batch)
			b.StopTimer()
			if err := store.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})

	b.Run("compact_target_update", func(b *testing.B) {
		b.ReportAllocs()
		b.ReportMetric(edgeCount, "edges/op")
		for i := 0; i < b.N; i++ {
			store := openUnresolvedTargetBenchmarkStore(b, fmt.Sprintf("compact-%d", i), identities)
			b.StartTimer()
			batch := make([]graph.UnresolvedEdgeTargetReindex, 0, len(identities))
			for pos, identity := range identities {
				batch = append(batch, graph.UnresolvedEdgeTargetReindex{Old: identity, NewTo: targets[pos]})
			}
			store.ReindexUnresolvedEdgeTargets(batch)
			b.StopTimer()
			if err := store.Close(); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func openUnresolvedTargetTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })
	return store
}

func openUnresolvedTargetBenchmarkStore(
	b *testing.B,
	name string,
	identities []graph.EdgeIdentity,
) *Store {
	b.Helper()
	store, err := Open(filepath.Join(b.TempDir(), name+".sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	payload := strings.Repeat("x", 2048)
	edges := make([]*graph.Edge, 0, len(identities))
	for _, identity := range identities {
		edges = append(edges, &graph.Edge{
			From: identity.From, To: identity.To, Kind: identity.Kind,
			FilePath: identity.FilePath, Line: identity.Line,
			Confidence: 0.8, ConfidenceLabel: "confirmed", Origin: "benchmark", Tier: "semantic",
			Meta: map[string]any{"payload": payload, "semantic_source": "benchmark"},
		})
	}
	store.AddBatch(nil, edges)
	return store
}

func unresolvedTargetStoredEdgeID(t *testing.T, store *Store, identity graph.EdgeIdentity) int64 {
	t.Helper()
	var id int64
	err := store.db.QueryRow(
		`SELECT id FROM edges WHERE from_id = ? AND to_id = ? AND kind = ? AND file_path = ? AND line = ?`,
		identity.From, identity.To, identity.Kind, identity.FilePath, identity.Line,
	).Scan(&id)
	require.NoError(t, err)
	return id
}
