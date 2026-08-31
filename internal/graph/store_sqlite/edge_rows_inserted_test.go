package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// The counter's whole reason to exist is that a pass's own edge total cannot
// distinguish "re-derived what was already there" from "discovered something".
// Every ingest is INSERT OR IGNORE, so re-emitting an identical edge is a no-op
// in the store while still counting toward the pass's report.
//
// This pins the property that makes the number readable: it advances by rows
// WRITTEN, not by edges offered. Without it, a near-zero `edge_rows_written` on
// a copied worktree could equally mean "the derive was redundant" or "the
// counter is broken", and that reading decides whether a pass gets deleted.
func TestEdgeRowsInsertedCountsWritesNotOffers(t *testing.T) {
	store := openReindexReceiptTestStore(t)
	nodes, edges := addBatchFixture(8, 4)

	require.Zero(t, store.EdgeRowsInserted(), "a fresh store has written nothing")

	store.AddBatch(nodes, edges)
	first := store.EdgeRowsInserted()
	require.EqualValues(t, len(edges), first,
		"the first ingest writes every edge offered")

	// The re-derive case: the same edges offered again, already present.
	store.AddBatch(nodes, edges)
	require.EqualValues(t, first, store.EdgeRowsInserted(),
		"re-offering edges the store already holds must not advance the counter — "+
			"this is exactly what a derive re-deriving a copied subgraph does, and "+
			"the pass's own `edges` total would report all of them again")

	// A genuinely new edge is the other half: the counter must still move, or a
	// derive that contributed real work would read as redundant.
	store.AddEdge(&graph.Edge{
		From: nodes[0].ID, To: nodes[1].ID,
		Kind: graph.EdgeCalls, FilePath: "novel.go", Line: 4242,
		Confidence: 0.9,
	})
	require.EqualValues(t, first+1, store.EdgeRowsInserted(),
		"a new edge advances the counter, including via AddEdge, which routes "+
			"through AddBatch rather than its own prepared statement")
}

// The capability is optional, and the indexer type-asserts for it rather than
// depending on this package. Pin that the assertion actually succeeds — a
// rename here would otherwise silently turn the measurement off.
func TestStoreSatisfiesEdgeRowsInsertedReporter(t *testing.T) {
	store := openReindexReceiptTestStore(t)

	got, ok := graph.EdgeRowsInserted(store)
	require.True(t, ok, "the sqlite store must satisfy graph.EdgeRowsInsertedReporter")
	require.Zero(t, got)
}
