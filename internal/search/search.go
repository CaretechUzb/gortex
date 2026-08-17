// Package search provides full-text search over code symbols with
// camelCase/snake_case-aware tokenization.
//
// The package owns no text index of its own. Search runs on
// SymbolSearcherBackend, a thin adapter over the graph store's own FTS
// index, and this package contributes the tokenization and query
// normalization both sides of that index agree on. A store that
// exposes no native symbol search gets NullBackend, whose empty corpus
// routes the query engine to its substring fallback.
//
// HybridBackend layers the optional vector channel on top of whichever
// text Backend it is given; Swappable lets the indexer replace the
// backend under a live engine.
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
// queries it so text and semantic (vector) ranks can contribute as
// separate signals instead of being collapsed via RRF before scoring.
// Backends that only do text search (the store-native FTS adapter)
// don't satisfy this interface; callers fall through to plain
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
