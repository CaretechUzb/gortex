package search

import (
	"strings"
	"sync/atomic"

	"github.com/zzet/gortex/internal/graph"
)

// SymbolSearcherBackend adapts a graph.SymbolSearcher into the
// search.Backend the daemon's search-symbols path consumes.
// Engine.gatherBackendCandidates and the rerank pipeline don't need
// to know whether the backend is BM25 / Bleve / native FTS — they
// see a plain search.Backend and call Search on it.
//
// Production wiring: when the indexer detects that the backing
// graph.Store also implements graph.SymbolSearcher, it constructs
// this adapter as the initial
// search.Backend wrapped by search.NewSwappable. The in-process
// Bleve / BM25 build path is then bypassed entirely.
//
// Add / Remove are no-ops on the adapter because the indexer
// already drives the SymbolSearcher writes directly:
//
//   - cold-load: BulkUpsertSymbolFTS at shadow-drain commit (see
//     internal/indexer.go IndexCtx defer)
//   - incremental: UpsertSymbolFTS alongside the parallel
//     idx.search.Add in the per-file path
//
// The adapter therefore only carries the read side. Callers that
// invoke Add / Remove still get the right behaviour because the
// indexer is the only entity that ever creates this adapter, and
// it doesn't rely on Add / Remove updating the FTS — those calls
// happen through the direct SymbolSearcher surface.
type SymbolSearcherBackend struct {
	s graph.SymbolSearcher

	// count tracks the indexer's incremental Add / Remove deltas
	// only — it does NOT report the actual size of the backend
	// FTS index (which lives in the disk store and is queryable
	// via the SymbolSearcher's own primitives). Used for the
	// search.Backend.Count() contract by callers that just want a
	// rough magnitude (no caller currently treats this as
	// authoritative).
	count atomic.Int64
}

// NewSymbolSearcherBackend wraps a SymbolSearcher in the
// search.Backend contract. The caller is responsible for keeping
// the underlying SymbolSearcher alive — Close on this adapter is
// a no-op and never touches the wrapped store.
func NewSymbolSearcherBackend(s graph.SymbolSearcher) *SymbolSearcherBackend {
	return &SymbolSearcherBackend{s: s}
}

// SymbolBundle re-exports graph.SymbolBundle so callers (the query
// engine, the rerank seed path) can construct + consume bundles
// without re-importing the graph package next to the search
// package import — symmetric with how SearchResult sits in
// search/.
type SymbolBundle = graph.SymbolBundle

// SearchSymbolBundles is the bundled-search hot path: it forwards
// to the wrapped graph.SymbolBundleSearcher when the underlying
// store implements that capability, returning the matched node +
// score + in/out edges in one engine round-trip. When the store
// only implements SymbolSearcher (no Bundle support), this method
// returns nil — callers MUST check the result and fall back to the
// per-call Search → GetNodesByIDs → GetIn/OutEdgesByNodeIDs path.
//
// Exposed on SymbolSearcherBackend (the production search.Backend
// adapter used in production) so the engine can type-assert through
// the search.Backend chain via SymbolBundleSearcherBackend without
// touching the daemon's wiring.
func (b *SymbolSearcherBackend) SearchSymbolBundles(query string, limit int) []SymbolBundle {
	if b == nil || b.s == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	bs, ok := b.s.(graph.SymbolBundleSearcher)
	if !ok {
		return nil
	}
	bundles, err := bs.SearchSymbolBundles(query, limit)
	if err != nil {
		return nil
	}
	return bundles
}

// SymbolBundleSearcherBackend is the interface the engine type-asserts
// on a search.Backend to detect bundle support. Both
// *SymbolSearcherBackend and *HybridBackend implement this; Swappable
// forwards.
type SymbolBundleSearcherBackend interface {
	SearchSymbolBundles(query string, limit int) []SymbolBundle
}

// SearchSymbolBundlesScoped is the repo-narrowed bundle path — the
// narrowing runs inside the wrapped store's FTS query (see
// graph.ScopedSymbolBundleSearcher for why a post-fetch filter can't
// substitute). Returns nil when the store has no scoped support;
// callers fall back to the unscoped bundle path.
func (b *SymbolSearcherBackend) SearchSymbolBundlesScoped(query string, repoAllow []string, limit int) []SymbolBundle {
	if b == nil || b.s == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	bs, ok := b.s.(graph.ScopedSymbolBundleSearcher)
	if !ok {
		return nil
	}
	bundles, err := bs.SearchSymbolBundlesRepoScoped(query, repoAllow, limit)
	if err != nil {
		return nil
	}
	return bundles
}

// ScopedSymbolBundleSearcherBackend is the engine's detection
// interface for the repo-narrowed bundle path. *SymbolSearcherBackend
// implements it; Swappable and HybridBackend forward.
type ScopedSymbolBundleSearcherBackend interface {
	SearchSymbolBundlesScoped(query string, repoAllow []string, limit int) []SymbolBundle
}

// Search forwards to SymbolSearcher.SearchSymbols and translates
// the per-hit (NodeID, Score) into search.SearchResult so callers
// don't see the graph package at all.
//
// An error from the backend is downgraded to an empty result — the
// daemon's search_symbols path already tolerates an empty primary
// hit set (it falls through to the exact-name / substring tiers in
// query.Engine.gatherBackendCandidates), so returning an error
// surface here would force every caller to grow its own fallback.
func (b *SymbolSearcherBackend) Search(query string, limit int) []SearchResult {
	if b == nil || b.s == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	hits, err := b.s.SearchSymbols(query, limit)
	if err != nil || len(hits) == 0 {
		return nil
	}
	out := make([]SearchResult, len(hits))
	for i, h := range hits {
		out[i] = SearchResult{ID: h.NodeID, Score: h.Score}
	}
	return out
}

// Add is a no-op — the indexer drives UpsertSymbolFTS on the wrapped
// SymbolSearcher directly. count is bumped so the Count() figure
// tracks the deltas-since-construction (best-effort, not
// authoritative — the disk index may be larger from a prior cold
// load).
func (b *SymbolSearcherBackend) Add(id string, _ ...string) {
	if b == nil || id == "" {
		return
	}
	b.count.Add(1)
}

// Remove is a no-op for the same reason as Add — the per-call
// removal path (when one lands) routes through SymbolSearcher
// directly, not through the search.Backend contract. count is
// decremented so the Count() figure stays roughly consistent.
func (b *SymbolSearcherBackend) Remove(id string) {
	if b == nil || id == "" {
		return
	}
	b.count.Add(-1)
}

// Count returns the running delta-since-construction. Used for
// observability / "is the index populated?" gates — never as a
// load-bearing decision input. The authoritative size lives in
// the disk FTS index, which is queryable via the
// SymbolSearcher's native primitives if needed.
func (b *SymbolSearcherBackend) Count() int {
	if b == nil {
		return 0
	}
	return int(b.count.Load())
}

// DocCount returns the authoritative number of indexed documents, straight
// from the underlying index, and reports whether it could be obtained.
//
// Count() must not be used for this: it is a delta of the indexer's Add and
// Remove calls since construction, so it is not a corpus size and can be
// negative. Anything user-facing asks here and omits the figure when the
// answer is unavailable, rather than printing the delta.
func (b *SymbolSearcherBackend) DocCount() (int, bool) {
	if b == nil || b.s == nil {
		return 0, false
	}
	counter, ok := b.s.(graph.SymbolFTSCounter)
	if !ok {
		return 0, false
	}
	count, err := counter.SymbolFTSCount()
	if err != nil || count < 0 {
		return 0, false
	}
	return count, true
}

// Close is a no-op. The wrapped SymbolSearcher is owned by the
// graph.Store; closing it from the search adapter would race the
// indexer's own lifecycle.
func (b *SymbolSearcherBackend) Close() {}
