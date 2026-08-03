package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type cloneCorpusSignatureWriterTestStore struct {
	graph.Store
	page            graph.CloneCorpusRow
	pageReads       int
	fullWrites      int
	signatureWrites int
	updates         []graph.CloneCorpusSignatureUpdate
}

func (s *cloneCorpusSignatureWriterTestStore) CloneCorpusPage(_ string, after string, _ int) ([]graph.CloneCorpusRow, error) {
	s.pageReads++
	if after != "" {
		return nil, nil
	}
	row := s.page
	row.Shingles = append([]uint64(nil), row.Shingles...)
	return []graph.CloneCorpusRow{row}, nil
}

func (s *cloneCorpusSignatureWriterTestStore) BulkSetCloneCorpus(string, []graph.CloneCorpusRow) error {
	s.fullWrites++
	return nil
}

func (s *cloneCorpusSignatureWriterTestStore) BulkSetCloneSignatures(_ string, updates []graph.CloneCorpusSignatureUpdate) error {
	s.signatureWrites++
	s.updates = append(s.updates, updates...)
	return nil
}

type cloneCorpusFullWriterTestStore struct {
	graph.Store
	page       graph.CloneCorpusRow
	fullWrites int
	wrote      []graph.CloneCorpusRow
}

func (s *cloneCorpusFullWriterTestStore) CloneCorpusPage(_ string, after string, _ int) ([]graph.CloneCorpusRow, error) {
	if after != "" {
		return nil, nil
	}
	row := s.page
	row.Shingles = append([]uint64(nil), row.Shingles...)
	return []graph.CloneCorpusRow{row}, nil
}

func (s *cloneCorpusFullWriterTestStore) BulkSetCloneCorpus(_ string, rows []graph.CloneCorpusRow) error {
	s.fullWrites++
	s.wrote = append(s.wrote, rows...)
	return nil
}

func pendingCloneCorpusRow() graph.CloneCorpusRow {
	return graph.CloneCorpusRow{
		NodeID: "repo/file.go::work", RepoPrefix: "repo",
		Shingles: []uint64{11, 22, 33}, TokenCount: 17,
	}
}

func TestFinaliseCloneCorpusUsesSignatureOnlyWriter(t *testing.T) {
	store := &cloneCorpusSignatureWriterTestStore{Store: graph.New(), page: pendingCloneCorpusRow()}
	baseline := finaliseCloneCorpusCtx(context.Background(), store, "repo")
	if !baseline.complete || baseline.itemCount != 1 || baseline.corpus != 1 {
		t.Fatalf("baseline = %+v", baseline)
	}
	if store.fullWrites != 0 || store.signatureWrites != 1 {
		t.Fatalf("full writes=%d signature writes=%d, want 0,1", store.fullWrites, store.signatureWrites)
	}
	if len(store.updates) != 1 || store.updates[0].NodeID != store.page.NodeID || store.updates[0].Signature == "" {
		t.Fatalf("signature updates = %+v", store.updates)
	}
}

func TestFinaliseCloneCorpusKeepsFullWriterFallback(t *testing.T) {
	store := &cloneCorpusFullWriterTestStore{Store: graph.New(), page: pendingCloneCorpusRow()}
	baseline := finaliseCloneCorpusCtx(context.Background(), store, "repo")
	if !baseline.complete || baseline.itemCount != 1 || baseline.corpus != 1 {
		t.Fatalf("baseline = %+v", baseline)
	}
	if store.fullWrites != 1 || len(store.wrote) != 1 {
		t.Fatalf("full writes=%d rows=%d, want 1,1", store.fullWrites, len(store.wrote))
	}
	if len(store.wrote[0].Shingles) != len(store.page.Shingles) || !store.wrote[0].Finalized || store.wrote[0].Signature == "" {
		t.Fatalf("fallback row = %+v", store.wrote[0])
	}
}
