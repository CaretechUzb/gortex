package indexer

import (
	"errors"
	"fmt"
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

// errInjectedFTSWrite stands in for whatever ends a rebuild half-way.
var errInjectedFTSWrite = errors.New("injected symbol FTS write failure")

// ftsFailingStore fails the second symbol-FTS write of a rebuild, through
// whichever door the indexer uses to perform it.
type ftsFailingStore struct {
	*store_sqlite.Store
	writes int
}

func (f *ftsFailingStore) failed() bool {
	f.writes++
	return f.writes >= 2
}

func (f *ftsFailingStore) BatchUpsertSymbolFTS(items []graph.SymbolFTSItem) error {
	if f.failed() {
		return errInjectedFTSWrite
	}
	return f.Store.BatchUpsertSymbolFTS(items)
}

func (f *ftsFailingStore) ReplaceSymbolFTS(repoPrefix string, produce func(emit func([]graph.SymbolFTSItem) error) error) error {
	return f.Store.ReplaceSymbolFTS(repoPrefix, func(emit func([]graph.SymbolFTSItem) error) error {
		return produce(func(items []graph.SymbolFTSItem) error {
			if f.failed() {
				return errInjectedFTSWrite
			}
			return emit(items)
		})
	})
}

// TestPopulateSymbolFTSKeepsPriorCorpusWhenRebuildFails is the production
// composition of the all-or-nothing rebuild. A rebuild that wipes the corpus
// and then appends chunk by chunk commits the wipe first, so a chunk that
// fails after it leaves the repository holding only what happened to land —
// documents that were searchable a moment ago now miss, and nothing repairs
// them short of another full index. The previous corpus must survive a
// rebuild that does not finish.
func TestPopulateSymbolFTSKeepsPriorCorpusWhenRebuildFails(t *testing.T) {
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "fts-rebuild.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	// The corpus the repository already has, indexed and being served.
	prior := make([]graph.SymbolFTSItem, 0, 8)
	for i := range 8 {
		prior = append(prior, graph.SymbolFTSItem{
			NodeID: fmt.Sprintf("prior_%02d.go::Prior%02d", i, i),
			Tokens: rebuildProbeToken("pri", i),
		})
	}
	require.NoError(t, store.BulkUpsertSymbolFTS("", prior))

	// Enough nodes that the rebuild streams more than one bounded chunk, so
	// the injected failure lands after a chunk has already been written.
	nodes := make([]*graph.Node, 0, symbolFTSDirectChunkRows+64)
	for i := range symbolFTSDirectChunkRows + 64 {
		nodes = append(nodes, &graph.Node{
			ID:       fmt.Sprintf("rebuilt_%04d.go::Rebuilt%04d", i, i),
			Kind:     graph.KindFunction,
			Name:     fmt.Sprintf("Rebuilt%04d", i),
			Language: "go",
			FilePath: fmt.Sprintf("rebuilt_%04d.go", i),
		})
	}
	store.AddBatch(nodes, nil)

	failing := &ftsFailingStore{Store: store}
	idx := newTestIndexer(failing)
	err = idx.populateSymbolFTS(nil)
	require.ErrorIs(t, err, errInjectedFTSWrite, "a failed FTS write must fail the rebuild")
	require.GreaterOrEqual(t, failing.writes, 2, "the failure must land after a chunk was already written")

	count, err := store.SymbolFTSCount()
	require.NoError(t, err)
	require.Equal(t, len(prior), count, "a failed rebuild must leave the prior corpus size untouched")
	for i, item := range prior {
		hits, err := store.SearchSymbols(rebuildProbeToken("pri", i), 20)
		require.NoError(t, err)
		ids := make([]string, 0, len(hits))
		for _, hit := range hits {
			ids = append(ids, hit.NodeID)
		}
		require.Contains(t, ids, item.NodeID, "a failed rebuild destroyed a previously searchable document")
	}
}

// rebuildProbeToken builds a fixed-width nonsense word unique to (family, n).
// The FTS query path ORs prefix terms together, so a per-document probe needs
// a token nothing else in the corpus carries.
func rebuildProbeToken(family string, n int) string {
	letters := [3]byte{}
	for i := 2; i >= 0; i-- {
		letters[i] = byte('a' + n%26)
		n /= 26
	}
	return family + string(letters[:])
}
