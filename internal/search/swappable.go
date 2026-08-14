package search

import (
	"sync"

	"github.com/zzet/gortex/internal/embedding"
)

// Swappable wraps a Backend and lets an in-place swap be performed
// concurrently with reads. Used by the indexer to re-wrap the active
// text backend in a HybridBackend once the vector index is ready,
// without making every call site re-thread a new Backend reference and
// without holding the indexer's lock across the swap.
//
// Callers see a stable *Swappable; reads delegate to whichever inner
// backend is currently active. ReplaceHybridVector atomically replaces the
// vector channel while preserving ownership of the live text backend.
type Swappable struct {
	mu    sync.RWMutex
	inner Backend

	// vectorUpdateMu serializes the durable corpus replacement and matching
	// process-local publication as one logical operation. Multi-repository
	// Indexers share this Swappable, so the lock prevents an older install from
	// publishing stale aggregate statistics after a newer store commit.
	vectorUpdateMu sync.Mutex
}

// NewSwappable wraps b. Panics if b is nil — every Indexer must start
// with a real backend, even if it's the in-memory NewAuto() default.
func NewSwappable(b Backend) *Swappable {
	if b == nil {
		panic("search.NewSwappable: nil backend")
	}
	return &Swappable{inner: b}
}

// ReplaceHybridVector atomically publishes a new vector channel while
// retaining the active text backend. Acquiring the write lock first drains all
// readers of the old backend. Existing hybrid layers are then peeled under the
// lock, transferring their text ownership into exactly one replacement hybrid.
// After publication, the retired hybrids release only their vector state.
//
// This operation is the safe alternative to composing an unpinned backend
// snapshot with NewHybrid: that sequence can race a reader, nest hybrids, and close the text
// backend that the replacement just reused. ReplaceHybridVector panics when
// vector is nil or the Swappable has already been closed.
func (s *Swappable) ReplaceHybridVector(vector *VectorBackend, embedder embedding.Provider) {
	if vector == nil {
		panic("search.Swappable.ReplaceHybridVector: nil vector backend")
	}

	s.mu.Lock()
	if s.inner == nil {
		s.mu.Unlock()
		panic("search.Swappable.ReplaceHybridVector: closed swappable")
	}

	text := s.inner
	retired := make([]*HybridBackend, 0, 1)
	for {
		hybrid, ok := text.(*HybridBackend)
		if !ok {
			break
		}
		retired = append(retired, hybrid)
		text = hybrid.TextBackend()
	}
	if text == nil {
		s.mu.Unlock()
		panic("search.Swappable.ReplaceHybridVector: hybrid has no text backend")
	}
	for _, hybrid := range retired {
		hybrid.detachTextBackend()
	}
	s.inner = NewHybrid(text, vector, embedder)
	s.mu.Unlock()

	for _, hybrid := range retired {
		hybrid.Close()
	}
}

// SerializeVectorUpdate runs fn while holding the publication lane shared by
// every Indexer that uses this Swappable. The callback should prepare expensive
// embeddings before entering this lane, then perform only the durable atomic
// corpus replacement and ReplaceHybridVector publication while it is held.
func (s *Swappable) SerializeVectorUpdate(fn func() error) error {
	if fn == nil {
		return nil
	}
	s.vectorUpdateMu.Lock()
	defer s.vectorUpdateMu.Unlock()
	return fn()
}

// AcquireBackend pins the currently active backend until release is called.
// Callers that need a capability not forwarded by Swappable must defer the
// returned release immediately and must not retain the backend beyond that
// scope. Holding the pin prevents ReplaceHybridVector and Close from retiring
// the backend underneath the caller. release is idempotent.
func (s *Swappable) AcquireBackend() (backend Backend, release func()) {
	s.mu.RLock()
	var once sync.Once
	return s.inner, func() {
		once.Do(s.mu.RUnlock)
	}
}

// Inner returns an unpinned snapshot of the currently-active backend. It is
// retained for tests and diagnostics only: production callers must not keep or
// dereference the result because replacement may retire it immediately after
// this method returns. Use AcquireBackend or a forwarded capability instead.
func (s *Swappable) Inner() Backend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inner
}

// Embedder returns the active hybrid's externally-owned embedding provider.
// The lookup is protected by the Swappable read lock; replacement never closes
// the provider, so the returned provider remains under its original owner's
// lifecycle rather than the retired HybridBackend's lifecycle.
func (s *Swappable) Embedder() embedding.Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	type embedderProvider interface {
		Embedder() embedding.Provider
	}
	if provider, ok := s.inner.(embedderProvider); ok {
		return provider.Embedder()
	}
	return nil
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
	// Hold the read lock for the complete call. Replacement owns and retires
	// the old backend after acquiring the write lock, so merely snapshotting
	// the pointer here would let it close underneath a live store query.
	s.mu.RLock()
	defer s.mu.RUnlock()
	if dc, ok := s.inner.(DocCounter); ok {
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
	// Match the vector-update lock order (publication lane, then backend lock)
	// so shutdown cannot retire the Swappable midway through a durable corpus
	// commit/publication callback.
	s.vectorUpdateMu.Lock()
	defer s.vectorUpdateMu.Unlock()
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
