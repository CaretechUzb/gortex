package indexer

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// countingSearch records Add/Remove calls so a test can assert that a node
// admitted by shouldIndexForSearch is the only kind ever removed. Production
// backends differ in how they store search state, but their admit and eviction
// predicates must remain symmetric.
type countingSearch struct {
	added   []string
	removed []string
}

func (c *countingSearch) Add(id string, fields ...string)          { c.added = append(c.added, id) }
func (c *countingSearch) Remove(id string)                         { c.removed = append(c.removed, id) }
func (c *countingSearch) Search(string, int) []search.SearchResult { return nil }
func (c *countingSearch) Count() int                               { return len(c.added) - len(c.removed) }
func (c *countingSearch) SizeBytes() int                           { return 0 }
func (c *countingSearch) Close()                                   {}

func TestRemoveFromSearchOnlyEvictsWhatWouldBeIndexed(t *testing.T) {
	spy := &countingSearch{}
	idx := &Indexer{search: spy, config: config.IndexConfig{}}

	// Kinds shouldIndexForSearch refuses. KindLocal is the important one:
	// those bindings are persisted purely to satisfy dataflow FK
	// constraints, so they appear in the prior-node set of every Go file
	// and were being removed on every single reconcile.
	rejected := []*graph.Node{
		{ID: "f", Kind: graph.KindFile},
		{ID: "i", Kind: graph.KindImport},
		{ID: "l", Kind: graph.KindLocal},
		{ID: "b", Kind: graph.KindBuiltin},
	}
	for _, n := range rejected {
		require.False(t, idx.shouldIndexForSearch(n), "%s should not be indexable", n.ID)
		idx.removeFromSearch(n)
	}
	require.Empty(t, spy.removed,
		"a node the admit predicate rejects must never reach Remove")

	admitted := &graph.Node{ID: "pkg/a.go::Fn", Kind: graph.KindFunction, Name: "Fn", Language: "go"}
	require.True(t, idx.shouldIndexForSearch(admitted))
	idx.removeFromSearch(admitted)
	require.Equal(t, []string{"pkg/a.go::Fn"}, spy.removed)
}

// Search membership only stays stable if the two predicates agree. Simulating
// a reconcile — evict the prior nodes, re-add the fresh ones — must be neutral
// for an unchanged file.
func TestReconcileOfUnchangedFileIsCountNeutral(t *testing.T) {
	spy := &countingSearch{}
	idx := &Indexer{search: spy, config: config.IndexConfig{}}

	nodes := []*graph.Node{
		{ID: "pkg/a.go", Kind: graph.KindFile},
		{ID: "pkg/a.go::Fn", Kind: graph.KindFunction, Name: "Fn", Language: "go"},
		{ID: "pkg/a.go::Fn::err", Kind: graph.KindLocal, Name: "err", Language: "go"},
		{ID: "pkg/a.go::Fn::data", Kind: graph.KindLocal, Name: "data", Language: "go"},
		{ID: "pkg/a.go::T", Kind: graph.KindType, Name: "T", Language: "go"},
	}

	// Admit pass — what the indexer would have added.
	for _, n := range nodes {
		if idx.shouldIndexForSearch(n) {
			spy.Add(n.ID)
		}
	}
	afterIndex := spy.Count()
	require.Equal(t, 2, afterIndex, "only Fn and T are searchable")

	// Ten reconciles of an unchanged file: evict prior, re-add fresh.
	for range 10 {
		for _, n := range nodes {
			idx.removeFromSearch(n)
		}
		for _, n := range nodes {
			if idx.shouldIndexForSearch(n) {
				spy.Add(n.ID)
			}
		}
	}

	require.Equal(t, afterIndex, spy.Count(),
		"re-indexing an unchanged file must not move the count; a broader evict predicate drains it by the difference set each pass")
	require.GreaterOrEqual(t, spy.Count(), 0, "a document count must never go negative")
}
