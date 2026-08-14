package indexer

import (
	"context"
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

func TestBuildSearchIndexCtx_WithoutVectorsSkipsNodeCensus(t *testing.T) {
	store := &countingSearchStore{Store: graph.New()}
	backend := search.NewSwappable(search.NewSymbolSearcherBackend(nil))
	defer backend.Close()

	idx := &Indexer{graph: store, search: backend}
	assert.NoError(t, idx.buildSearchIndexCtx(context.Background()))

	assert.Zero(t, store.allNodesCalls)
}
