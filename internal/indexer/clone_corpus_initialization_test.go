package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type cloneCorpusInitializationTestStore struct {
	graph.Store
	initialized bool
	nodeReads   int
	marks       int
}

func (s *cloneCorpusInitializationTestStore) CloneCorpusPage(string, string, int) ([]graph.CloneCorpusRow, error) {
	return nil, nil
}

func (s *cloneCorpusInitializationTestStore) BulkSetCloneCorpus(string, []graph.CloneCorpusRow) error {
	return nil
}

func (s *cloneCorpusInitializationTestStore) CloneCorpusInitialized(string) (bool, error) {
	return s.initialized, nil
}

func (s *cloneCorpusInitializationTestStore) MarkCloneCorpusInitialized(string) error {
	s.marks++
	s.initialized = true
	return nil
}

func (s *cloneCorpusInitializationTestStore) GetRepoNodes(repoPrefix string) []*graph.Node {
	s.nodeReads++
	return s.Store.GetRepoNodes(repoPrefix)
}

type cloneCorpusAdapterTestStore struct {
	graph.Store
	nodeReads int
}

func (s *cloneCorpusAdapterTestStore) CloneCorpusPage(string, string, int) ([]graph.CloneCorpusRow, error) {
	return nil, nil
}

func (s *cloneCorpusAdapterTestStore) BulkSetCloneCorpus(string, []graph.CloneCorpusRow) error {
	return nil
}

func (s *cloneCorpusAdapterTestStore) GetRepoNodes(repoPrefix string) []*graph.Node {
	s.nodeReads++
	return s.Store.GetRepoNodes(repoPrefix)
}

func TestFinaliseCloneCorpusMarksLegacyEmptyOnce(t *testing.T) {
	store := &cloneCorpusInitializationTestStore{Store: graph.New()}

	first := finaliseCloneCorpusCtx(context.Background(), store, "repo")
	if !first.complete || first.corpus != 0 || first.itemCount != 0 {
		t.Fatalf("first baseline = %+v, want complete empty", first)
	}
	if store.nodeReads != 1 || store.marks != 1 {
		t.Fatalf("first pass reads=%d marks=%d, want 1,1", store.nodeReads, store.marks)
	}

	second := finaliseCloneCorpusCtx(context.Background(), store, "repo")
	if !second.complete || second.corpus != 0 || second.itemCount != 0 {
		t.Fatalf("second baseline = %+v, want complete empty", second)
	}
	if store.nodeReads != 1 || store.marks != 1 {
		t.Fatalf("second pass reads=%d marks=%d, want unchanged 1,1", store.nodeReads, store.marks)
	}
}

func TestFinaliseCloneCorpusAdapterKeepsLegacyFallback(t *testing.T) {
	store := &cloneCorpusAdapterTestStore{Store: graph.New()}

	for range 2 {
		baseline := finaliseCloneCorpusCtx(context.Background(), store, "repo")
		if !baseline.complete || baseline.corpus != 0 || baseline.itemCount != 0 {
			t.Fatalf("baseline = %+v, want complete empty", baseline)
		}
	}
	if store.nodeReads != 2 {
		t.Fatalf("node reads = %d, want adapter fallback on both calls", store.nodeReads)
	}
}
