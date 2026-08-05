package indexer

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// TestDirectColdPathPopulatesSymbolFTS covers the cold index that never gets an
// in-memory shadow: parsing runs straight against the disk store, so nothing in
// the mutation path writes the backend's symbol FTS. The corpus must still be
// there when the index returns, otherwise symbol search is degraded on exactly
// the repositories large enough to refuse the shadow.
func TestDirectColdPathPopulatesSymbolFTS(t *testing.T) {
	t.Setenv("GORTEX_SHADOW_MAX_FILES", "0") // refuse the shadow: direct disk parse
	repo := t.TempDir()
	writeFile(t, filepath.Join(repo, "orders.go"), `package shop

// ReconcileOutstandingOrders settles unpaid orders.
func ReconcileOutstandingOrders() int { return 0 }
`)
	writeFile(t, filepath.Join(repo, "shipping.go"), `package shop

// DispatchPendingShipment hands a parcel to the carrier.
func DispatchPendingShipment() error { return nil }
`)

	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "direct-cold.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	idx := newTestIndexer(store)
	_, err = idx.Index(repo)
	require.NoError(t, err)

	counter, ok := any(store).(graph.SymbolFTSCounter)
	require.True(t, ok, "sqlite store must expose the authoritative FTS count")
	count, err := counter.SymbolFTSCount()
	require.NoError(t, err)
	require.Greater(t, count, 0, "direct cold index left the symbol FTS empty")

	for query, wantID := range map[string]string{
		"ReconcileOutstandingOrders": "orders.go::ReconcileOutstandingOrders",
		"DispatchPendingShipment":    "shipping.go::DispatchPendingShipment",
	} {
		hits, err := store.SearchSymbols(query, 10)
		require.NoError(t, err)
		require.NotEmpty(t, hits, "no FTS hit for %q", query)
		ids := make([]string, 0, len(hits))
		for _, hit := range hits {
			ids = append(ids, hit.NodeID)
		}
		require.Contains(t, ids, wantID)
	}

	// The FTS corpus must carry the same admission rule as every other writer:
	// file nodes are never searchable symbols.
	for _, hit := range searchAll(t, store, "orders") {
		node := store.GetNode(hit.NodeID)
		require.NotNil(t, node)
		require.NotEqual(t, graph.KindFile, node.Kind, "file node leaked into the symbol corpus")
	}
}

func searchAll(t *testing.T, store graph.SymbolSearcher, query string) []graph.SymbolHit {
	t.Helper()
	hits, err := store.SearchSymbols(query, 50)
	require.NoError(t, err)
	return hits
}
