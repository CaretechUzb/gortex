package indexer

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/search"
)

type vectorPlanTestStore struct {
	graph.Store
	mu           sync.Mutex
	replaceErr   error
	replaceStats graph.VectorCorpusStats
	replaceCalls int
	hits         []graph.VectorHit
}

func newVectorPlanTestStore() *vectorPlanTestStore {
	return &vectorPlanTestStore{Store: graph.New()}
}

func (s *vectorPlanTestStore) ReplaceVectorCorpus(
	_ context.Context,
	_ string,
	dims int,
	_ []graph.VectorCorpusItem,
) (graph.VectorCorpusStats, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replaceCalls++
	if s.replaceErr != nil {
		return graph.VectorCorpusStats{}, s.replaceErr
	}
	stats := s.replaceStats
	if stats.Dims <= 0 {
		stats.Dims = dims
	}
	return stats, nil
}

func (s *vectorPlanTestStore) VectorCorpusStats(context.Context, int) (graph.VectorCorpusStats, error) {
	return s.replaceStats, nil
}

func (s *vectorPlanTestStore) VectorCorpusStatsForRepo(context.Context, string, int) (graph.VectorCorpusStats, error) {
	return s.replaceStats, nil
}

func (s *vectorPlanTestStore) UpsertEmbedding(string, []float32) error { return nil }
func (s *vectorPlanTestStore) BulkUpsertEmbeddings([]graph.VectorItem) error {
	return nil
}
func (s *vectorPlanTestStore) BuildVectorIndex(int) error { return nil }
func (s *vectorPlanTestStore) SimilarTo(_ []float32, limit int) ([]graph.VectorHit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < len(s.hits) {
		return append([]graph.VectorHit(nil), s.hits[:limit]...), nil
	}
	return append([]graph.VectorHit(nil), s.hits...), nil
}
func (s *vectorPlanTestStore) GetEmbeddings([]string) map[string][]float32 { return nil }

func vectorPlanTestIndexer(store graph.Store) *Indexer {
	idx := New(store, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetEmbedder(&poolEmbedder{})
	return idx
}

func assertVectorPlanReleased(t *testing.T, plan *preparedVectorPlan) {
	t.Helper()
	assert.Empty(t, plan.repoPrefix)
	assert.Zero(t, plan.dims)
	assert.Nil(t, plan.items)
	assert.Nil(t, plan.chunkMap)
	assert.Nil(t, plan.droppedIDs)
	assert.Zero(t, plan.dropped)
	assert.Zero(t, plan.embeddedText)
}

func TestPreparedVectorPlanReleaseClearsOwnedState(t *testing.T) {
	plan := &preparedVectorPlan{
		repoPrefix: "repo",
		dims:       3,
		items: []graph.VectorCorpusItem{
			{NodeID: "repo/a", Vec: []float32{1, 0, 0}},
		},
		chunkMap:     map[string]string{"repo/a#chunk0": "repo/a"},
		dropped:      1,
		droppedIDs:   []string{"repo/b"},
		embeddedText: 2,
	}

	plan.Release()
	assertVectorPlanReleased(t, plan)
	plan.Release() // idempotent
}

func TestInstallVectorPlanPublishesDelegatedBackendAndReleasesPlan(t *testing.T) {
	store := newVectorPlanTestStore()
	store.replaceStats = graph.VectorCorpusStats{
		VectorCount: 2, ChunkCount: 1,
		RepositoryVectorCount: 2, RepositoryChunkCount: 1,
		Dims: 3,
	}
	idx := vectorPlanTestIndexer(store)
	plan := &preparedVectorPlan{
		repoPrefix: "repo",
		dims:       3,
		items: []graph.VectorCorpusItem{
			{NodeID: "repo/a", Vec: []float32{1, 0, 0}},
			{NodeID: "repo/b#chunk0", ParentID: "repo/b", Vec: []float32{0, 1, 0}},
		},
		chunkMap: map[string]string{"repo/b#chunk0": "repo/b"},
	}

	require.NoError(t, idx.installVectorPlan(context.Background(), store, plan))
	assertVectorPlanReleased(t, plan)
	require.Equal(t, 1, store.replaceCalls)

	backend, release := idx.swappable().AcquireBackend()
	defer release()
	hybrid, ok := backend.(*search.HybridBackend)
	require.True(t, ok)
	require.Equal(t, 2, hybrid.VectorIndex().Count())
	require.True(t, hybrid.VectorIndex().HasChunks())
	require.Zero(t, hybrid.VectorSizeBytes())
}

func TestInstallVectorPlanCancellationKeepsPublicationAndReleasesPlan(t *testing.T) {
	store := newVectorPlanTestStore()
	idx := vectorPlanTestIndexer(store)
	before, release := idx.swappable().AcquireBackend()
	release()
	plan := &preparedVectorPlan{
		repoPrefix: "repo",
		dims:       3,
		items: []graph.VectorCorpusItem{
			{NodeID: "repo/a", Vec: []float32{1, 0, 0}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := idx.installVectorPlan(ctx, store, plan)
	require.ErrorIs(t, err, context.Canceled)
	assertVectorPlanReleased(t, plan)
	require.Zero(t, store.replaceCalls)
	after, release := idx.swappable().AcquireBackend()
	release()
	if after != before {
		t.Fatal("cancelled install changed the active search backend")
	}
}

func TestInstallVectorPlanNativeFailureUsesPreparedHeapWithoutReembedding(t *testing.T) {
	store := newVectorPlanTestStore()
	store.replaceErr = errors.New("native install failed")
	emb := &poolEmbedder{}
	idx := New(store, parser.NewRegistry(), config.Default().Index, zap.NewNop())
	idx.SetEmbedder(emb)
	plan := &preparedVectorPlan{
		repoPrefix: "repo",
		dims:       3,
		items: []graph.VectorCorpusItem{
			{NodeID: "repo/a", Vec: []float32{1, 0, 0}},
		},
	}

	require.NoError(t, idx.installVectorPlan(context.Background(), store, plan))
	assertVectorPlanReleased(t, plan)
	require.Zero(t, emb.calls, "heap fallback must reuse the plan without embedding")
	require.ErrorIs(t, idx.LastVectorBuildError(), store.replaceErr)

	backend, release := idx.swappable().AcquireBackend()
	defer release()
	hybrid, ok := backend.(*search.HybridBackend)
	require.True(t, ok)
	require.Equal(t, 1, hybrid.VectorIndex().Count())
	require.Greater(t, hybrid.VectorSizeBytes(), uint64(0))
}

func TestInstallVectorPlanMultiRepoFailurePreservesGlobalPublication(t *testing.T) {
	store := newVectorPlanTestStore()
	store.replaceStats = graph.VectorCorpusStats{VectorCount: 2, ChunkCount: 1, Dims: 3}
	idx := vectorPlanTestIndexer(store)
	initial := &preparedVectorPlan{
		repoPrefix: "A",
		dims:       3,
		items: []graph.VectorCorpusItem{
			{NodeID: "A/a", Vec: []float32{1, 0, 0}},
		},
	}
	require.NoError(t, idx.installVectorPlan(context.Background(), store, initial))
	before, release := idx.swappable().AcquireBackend()
	release()

	idx.repositoryMutationOwner = &MultiIndexer{}
	store.replaceErr = errors.New("repo B install failed")
	plan := &preparedVectorPlan{
		repoPrefix: "B",
		dims:       3,
		items: []graph.VectorCorpusItem{
			{NodeID: "B/b", Vec: []float32{0, 1, 0}},
		},
	}
	require.NoError(t, idx.installVectorPlan(context.Background(), store, plan))
	assertVectorPlanReleased(t, plan)
	require.ErrorIs(t, idx.LastVectorBuildError(), store.replaceErr)
	after, release := idx.swappable().AcquireBackend()
	release()
	if after != before {
		t.Fatal("a repository-local fallback replaced the shared global vector publication")
	}
}

type vectorPlanResultEmbedder struct {
	vectors [][]float32
}

func (e *vectorPlanResultEmbedder) Embed(context.Context, string) ([]float32, error) {
	if len(e.vectors) == 0 {
		return nil, nil
	}
	return e.vectors[0], nil
}
func (e *vectorPlanResultEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	return e.vectors, nil
}
func (e *vectorPlanResultEmbedder) Dimensions() int { return 3 }
func (e *vectorPlanResultEmbedder) Close() error    { return nil }

func TestPrepareVectorPlanRejectsCardinalityAndNonFiniteResults(t *testing.T) {
	nodes := []*graph.Node{{
		ID: "repo/a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha",
		FilePath: "repo/a.go", RepoPrefix: "repo", Language: "go",
	}}
	tests := []struct {
		name    string
		vectors [][]float32
		want    string
	}{
		{name: "missing", vectors: nil, want: "returned 0 vectors for 1 texts"},
		{name: "extra", vectors: [][]float32{{1, 0, 0}, {0, 1, 0}}, want: "returned 2 vectors for 1 texts"},
		{name: "nan", vectors: [][]float32{{float32(math.NaN()), 0, 0}}, want: "all 1 embedding vectors were invalid"},
		{name: "infinity", vectors: [][]float32{{float32(math.Inf(1)), 0, 0}}, want: "all 1 embedding vectors were invalid"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			idx := New(graph.New(), parser.NewRegistry(), config.Default().Index, zap.NewNop())
			idx.SetRepoPrefix("repo")
			idx.SetEmbedder(&vectorPlanResultEmbedder{vectors: tt.vectors})
			plan, err := idx.prepareVectorPlan(context.Background(), nodes)
			require.Error(t, err)
			require.Contains(t, err.Error(), tt.want)
			require.Nil(t, plan)
		})
	}
}
