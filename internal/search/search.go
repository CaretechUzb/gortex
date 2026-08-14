// Package search provides full-text search over code symbols with
// camelCase/snake_case-aware tokenization and BM25 ranking.
//
// Production search runs on SymbolSearcherBackend, a thin adapter over
// the graph store's own FTS index — no parallel in-process corpus.
// BM25Backend, a self-contained in-memory inverted index, is the
// fallback for stores that expose no native symbol search.
package search

// SearchResult is a single search hit.
type SearchResult struct {
	ID    string  `json:"id"`
	Score float64 `json:"score"`
}

// Backend is the interface for search backends.
type Backend interface {
	// Add indexes a symbol with the given text fields.
	Add(id string, fields ...string)

	// Remove deletes a symbol from the index.
	Remove(id string)

	// Search queries the index and returns ranked results.
	Search(query string, limit int) []SearchResult

	// Count returns the number of indexed documents.
	Count() int

	// Close releases resources.
	Close()
}

// ChannelSearcher is an optional interface a Backend can implement to
// expose its per-channel raw retrieval output. The rerank pipeline
// queries it so BM25 and semantic (vector) ranks can contribute as
// separate signals instead of being collapsed via RRF before scoring.
// Backends that only do text search (BM25, the store-native FTS
// adapter) don't satisfy this interface; callers fall through to plain
// Search().
type ChannelSearcher interface {
	SearchChannels(query string, limit int) (textResults []SearchResult, vectorIDs []string)
}

// Sizer is an optional interface a Backend can implement to report its
// approximate in-memory footprint. Used by `gortex daemon status` to
// break down per-repo memory; callers should type-assert and treat a
// missing implementation as zero.
type Sizer interface {
	SizeBytes() uint64
}

// BackendSize returns the estimated byte size of b if it implements
// Sizer, or zero otherwise. Safe to call on a nil Backend.
func BackendSize(b Backend) uint64 {
	if b == nil {
		return 0
	}
	if s, ok := b.(Sizer); ok {
		return s.SizeBytes()
	}
	return 0
}

// NewAuto returns the default in-process text backend. Reached only
// when the graph store exposes no native symbol search; otherwise the
// indexer wires up a SymbolSearcherBackend over the store's own FTS.
func NewAuto() Backend {
	return NewBM25()
}
