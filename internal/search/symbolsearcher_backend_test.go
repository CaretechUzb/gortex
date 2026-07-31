package search

import (
	"errors"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

type symbolSearcherStub struct{}

func (symbolSearcherStub) UpsertSymbolFTS(string, string) error { return nil }
func (symbolSearcherStub) BulkUpsertSymbolFTS(string, []graph.SymbolFTSItem) error {
	return nil
}
func (symbolSearcherStub) BuildSymbolIndex() error { return nil }
func (symbolSearcherStub) SearchSymbols(string, int) ([]graph.SymbolHit, error) {
	return nil, nil
}

type countedSymbolSearcherStub struct {
	symbolSearcherStub
	count int
	err   error
	calls int
}

func (s *countedSymbolSearcherStub) SymbolFTSCount() (int, error) {
	s.calls++
	return s.count, s.err
}

func TestNewSymbolSearcherBackendSeedsPersistedCountLazily(t *testing.T) {
	store := &countedSymbolSearcherStub{count: 37}
	backend := NewSymbolSearcherBackend(store)
	if store.calls != 0 {
		t.Fatalf("constructor SymbolFTSCount calls = %d, want 0", store.calls)
	}

	backend.Add("new-before-first-count")
	if got := backend.Count(); got != 38 {
		t.Fatalf("first Count() = %d, want persisted count plus prior delta 38", got)
	}
	if store.calls != 1 {
		t.Fatalf("SymbolFTSCount calls = %d, want 1", store.calls)
	}
	if got := backend.Count(); got != 38 {
		t.Fatalf("second Count() = %d, want cached count 38", got)
	}
	if store.calls != 1 {
		t.Fatalf("repeated Count SymbolFTSCount calls = %d, want 1", store.calls)
	}

	backend.Remove("new-before-first-count")
	if got := backend.Count(); got != 37 {
		t.Fatalf("Count() after balanced delta = %d, want 37", got)
	}
}

func TestNewSymbolSearcherBackendLeavesCountEmptyWithoutPersistedCorpus(t *testing.T) {
	counterErr := errors.New("count unavailable")
	tests := []struct {
		name  string
		store graph.SymbolSearcher
	}{
		{name: "nil store", store: nil},
		{name: "counter unsupported", store: symbolSearcherStub{}},
		{name: "empty corpus", store: &countedSymbolSearcherStub{}},
		{name: "invalid negative count", store: &countedSymbolSearcherStub{count: -1}},
		{name: "counter error", store: &countedSymbolSearcherStub{count: 41, err: counterErr}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := NewSymbolSearcherBackend(tt.store)
			if got := backend.Count(); got != 0 {
				t.Fatalf("Count() = %d, want 0", got)
			}
		})
	}
}
