package indexer

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

type countingSearchStore struct {
	graph.Store
	allNodesCalls int
}

func (s *countingSearchStore) AllNodes() []*graph.Node {
	s.allNodesCalls++
	return s.Store.AllNodes()
}

func TestBuildSearchIndex_NativeTextWithoutVectorsSkipsNodeCensus(t *testing.T) {
	store := &countingSearchStore{Store: graph.New()}
	backend := search.NewSwappable(search.NewSymbolSearcherBackend(nil))
	defer backend.Close()

	idx := &Indexer{graph: store, search: backend}
	idx.buildSearchIndex()

	assert.Zero(t, store.allNodesCalls)
}

func TestIsSymbolSearcherBackend_UnwrapsProductionLayers(t *testing.T) {
	native := search.NewSymbolSearcherBackend(nil)
	assert.True(t, isSymbolSearcherBackend(native))

	swappable := search.NewSwappable(native)
	assert.True(t, isSymbolSearcherBackend(swappable))

	hybrid := search.NewHybrid(native, search.NewVector(1), nil)
	assert.True(t, isSymbolSearcherBackend(hybrid))
	assert.True(t, isSymbolSearcherBackend(search.NewSwappable(hybrid)))

	bm25 := search.NewBM25()
	defer bm25.Close()
	assert.False(t, isSymbolSearcherBackend(bm25))
}
