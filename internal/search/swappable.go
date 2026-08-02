package search

import "sync"

// Swappable wraps a Backend and lets a single in-place swap be performed
// concurrently with reads. Used by the indexer to upgrade from the
// in-memory BM25 backend to Bleve once the corpus crosses AutoThreshold,
// without making every call site re-thread a new Backend reference and
// without holding the indexer's lock during the (potentially seconds-long)
// re-population of Bleve.
//
// Callers see a stable *Swappable; reads delegate to whichever inner
// backend is currently active. Swap atomically replaces the inner
// backend and closes the previous one.
type Swappable struct {
	mu    sync.RWMutex
	inner Backend
}

// NewSwappable wraps b. Panics if b is nil — every Indexer must start
// with a real backend, even if it's the in-memory NewAuto() default.
func NewSwappable(b Backend) *Swappable {
	if b == nil {
		panic("search.NewSwappable: nil backend")
	}
	return &Swappable{inner: b}
}

// Swap installs the new backend and closes the old one. Safe to call
// concurrently with reads; the swap itself is brief (one pointer write
// under the write lock) so reads queued during the swap return promptly
// against the new backend.
func (s *Swappable) Swap(b Backend) {
	s.mu.Lock()
	old := s.inner
	s.inner = b
	s.mu.Unlock()
	if old != nil && old != b {
		old.Close()
	}
}

// Inner returns the currently-active backend. Used internally to test
// upgrade outcomes; production code should always go through the
// Backend interface methods on Swappable itself.
func (s *Swappable) Inner() Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner
}

// --- Backend interface ------------------------------------------------

func (s *Swappable) Add(id string, fields ...string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.inner.Add(id, fields...)
}

func (s *Swappable) Remove(id string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	s.inner.Remove(id)
}

func (s *Swappable) Search(query string, limit int) []SearchResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner.Search(query, limit)
}

// SearchChannels delegates to the inner backend when it implements
// ChannelSearcher, so a HybridBackend wrapped in a Swappable still
// exposes per-channel rank data to the rerank pipeline.
func (s *Swappable) SearchChannels(query string, limit int) (textResults []SearchResult, vectorIDs []string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cs, ok := s.inner.(ChannelSearcher); ok {
		return cs.SearchChannels(query, limit)
	}
	return s.inner.Search(query, limit), nil
}

// SearchChannelsTimed delegates to a backend that supports the
// per-phase timing breakdown (today only HybridBackend). Falls back
// to SearchChannels — and a zero-valued ChannelTimings — when the
// inner backend doesn't know how to split phases.
func (s *Swappable) SearchChannelsTimed(query string, limit int) ([]SearchResult, []string, ChannelTimings) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type timer interface {
		SearchChannelsTimed(query string, limit int) ([]SearchResult, []string, ChannelTimings)
	}
	if cst, ok := s.inner.(timer); ok {
		return cst.SearchChannelsTimed(query, limit)
	}
	if cs, ok := s.inner.(ChannelSearcher); ok {
		text, vec := cs.SearchChannels(query, limit)
		return text, vec, ChannelTimings{}
	}
	return s.inner.Search(query, limit), nil, ChannelTimings{}
}

// SearchSymbolBundles forwards to the inner backend when it implements
// SymbolBundleSearcherBackend (production wiring: a
// SymbolSearcherBackend whose store is the disk Store, or a
// HybridBackend whose text backend is the same). Returns nil when the
// inner backend doesn't expose bundles — the engine treats nil as
// "no bundle support" and falls back to the per-call Search +
// GetNodesByIDs + GetIn/OutEdgesByNodeIDs path.
func (s *Swappable) SearchSymbolBundles(query string, limit int) []SymbolBundle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if bs, ok := s.inner.(SymbolBundleSearcherBackend); ok {
		return bs.SearchSymbolBundles(query, limit)
	}
	return nil
}

// DocCount forwards the authoritative corpus size when the inner
// backend exposes it — the engine's has-corpus gate reads this so a
// warm-started daemon over a populated disk FTS isn't mistaken for an
// empty backend.
func (s *Swappable) DocCount() (int, bool) {
	// Snapshot the inner backend under the lock but run the count
	// outside it — DocCount can be a real store query and holding
	// the RLock across it would stall a pending Swap (and, behind
	// it, every new reader).
	s.mu.RLock()
	inner := s.inner
	s.mu.RUnlock()
	if dc, ok := inner.(DocCounter); ok {
		return dc.DocCount()
	}
	return 0, false
}

// SearchSymbolBundlesScoped forwards the repo-narrowed bundle path the
// same way; nil when the inner backend doesn't expose it.
func (s *Swappable) SearchSymbolBundlesScoped(query string, repoAllow []string, limit int) []SymbolBundle {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if bs, ok := s.inner.(ScopedSymbolBundleSearcherBackend); ok {
		return bs.SearchSymbolBundlesScoped(query, repoAllow, limit)
	}
	return nil
}

// VectorChannelOnly forwards to the inner backend when it implements
// the vector-only channel pull (today: HybridBackend). Lets the
// engine fetch the vector channel without re-running text BM25 —
// the bundle path already has the text hits. Returns (nil, zero
// timings) when the inner backend isn't vector-aware.
func (s *Swappable) VectorChannelOnly(query string, limit int) ([]string, ChannelTimings) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type vco interface {
		VectorChannelOnly(query string, limit int) ([]string, ChannelTimings)
	}
	if v, ok := s.inner.(vco); ok {
		return v.VectorChannelOnly(query, limit)
	}
	return nil, ChannelTimings{}
}

func (s *Swappable) Count() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner.Count()
}

func (s *Swappable) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inner != nil {
		s.inner.Close()
		s.inner = nil
	}
}

// SizeBytes delegates to the inner backend's SizeBytes implementation
// if it provides one; otherwise zero.
func (s *Swappable) SizeBytes() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return BackendSize(s.inner)
}
