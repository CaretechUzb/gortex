package search

import (
	"sync"

	"github.com/coder/hnsw"

	"github.com/zzet/gortex/internal/graph"
)

// VectorDelegate is the subset of graph.VectorSearcher the
// VectorBackend shim consults when it's been told to delegate
// instead of holding an in-process HNSW. Exported (with a
// graph.VectorHit return) so the indexer can install a delegate
// without writing a translation layer — search already depends on
// graph for SymbolHit, so the type sharing is free.
type VectorDelegate interface {
	SimilarTo(vec []float32, limit int) ([]graph.VectorHit, error)
}

// VectorBackend stores and searches embedding vectors using HNSW index.
//
// When delegate is set (via SetDelegate), the in-process HNSW is
// bypassed entirely: Add becomes a no-op (the indexer drives the
// delegate's bulk-upsert directly), Search forwards to the
// delegate's SimilarTo. The dims and chunkMap stay live so callers
// that need them (HybridBackend.dechunkVectorIDs) keep working
// against the same VectorBackend surface.
type VectorBackend struct {
	graph *hnsw.Graph[string]
	count int
	dims  int
	// chunkMap maps a synthetic chunk vector ID ("<symbolID>#chunkK")
	// to its parent symbol ID. It is non-empty only when AST
	// sub-chunking split one or more symbols into multiple vectors.
	// Search results are de-chunked through it so a symbol is never
	// returned twice and chunk IDs never leak to callers.
	chunkMap map[string]string
	mu       sync.RWMutex

	// delegate is the optional engine-native vector searcher (today
	// only graph.SymbolSearcher-implementing stores). Set means
	// "don't build the in-process HNSW; route reads through here".
	// The wrapped delegateCount tracks Add-call deltas so Count()
	// reports a non-zero figure once the indexer has finished its
	// bulk upsert — HybridBackend gates the vector channel on
	// Count() > 0.
	delegate      VectorDelegate
	delegateCount int
}

// NewVector creates a vector search backend for the given embedding dimensions.
func NewVector(dims int) *VectorBackend {
	g := hnsw.NewGraph[string]()
	g.Distance = hnsw.CosineDistance
	return &VectorBackend{
		graph: g,
		dims:  dims,
	}
}

// SetChunkMap installs the chunk-vector → parent-symbol mapping. Called
// by the indexer after a chunked vector build. A nil or empty map
// means no symbol was sub-chunked.
func (v *VectorBackend) SetChunkMap(m map[string]string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.chunkMap = m
}

// ResolveChunk maps a vector ID to the symbol ID a caller should see.
// For a chunk vector it returns the parent symbol ID; for a plain
// symbol vector it returns the ID unchanged. The bool reports whether
// the ID was a chunk.
func (v *VectorBackend) ResolveChunk(id string) (string, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if parent, ok := v.chunkMap[id]; ok {
		return parent, true
	}
	return id, false
}

// HasChunks reports whether any vector in the index is a chunk.
func (v *VectorBackend) HasChunks() bool {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.chunkMap) > 0
}

// Add indexes a symbol with its embedding vector.
func (v *VectorBackend) Add(id string, vector []float32) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.delegate != nil {
		// Delegated mode: the indexer pushes vectors to the
		// engine-native HNSW via the graph.VectorSearcher
		// interface directly. Add here is a no-op so the
		// in-process hnsw.Graph never allocates memory for what
		// the delegate already owns; count tracks deltas so
		// Count()'s "is the index populated" gate fires.
		v.delegateCount++
		return
	}
	v.graph.Add(hnsw.Node[string]{
		Key:   id,
		Value: hnsw.Vector(vector),
	})
	v.count++
}

// SetDelegate routes Search / Count through an engine-native vector
// searcher (the disk store's graph.VectorSearcher). After
// the call:
//   - Add is a no-op (the indexer talks to the delegate directly via
//     graph.VectorSearcher.BulkUpsertEmbeddings / UpsertEmbedding),
//   - Search forwards to delegate.SimilarTo,
//   - Count reflects the delegate-delta count (not the in-process
//     graph), so HybridBackend.searchChannels's `v.Count() > 0` gate
//     fires once the indexer has populated the backend.
func (v *VectorBackend) SetDelegate(d VectorDelegate) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.delegate = d
}

// Search returns the k nearest neighbors to the query vector.
func (v *VectorBackend) Search(query []float32, k int) []string {
	v.mu.RLock()
	d := v.delegate
	v.mu.RUnlock()
	if d != nil {
		hits, err := d.SimilarTo(query, k)
		if err != nil || len(hits) == 0 {
			return nil
		}
		ids := make([]string, len(hits))
		for i, h := range hits {
			ids[i] = h.NodeID
		}
		return ids
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.count == 0 {
		return nil
	}
	results := v.graph.Search(hnsw.Vector(query), k)
	ids := make([]string, len(results))
	for i, r := range results {
		ids[i] = r.Key
	}
	return ids
}

// Count returns the number of indexed vectors.
func (v *VectorBackend) Count() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.delegate != nil {
		return v.delegateCount
	}
	return v.count
}

// Dims returns the embedding dimensionality.
func (v *VectorBackend) Dims() int { return v.dims }

// SizeBytes estimates HNSW's heap footprint. The raw vector storage
// (dims × 4 B) is a small fraction of the total — each node also
// carries layer neighbor lists, priority-queue scratch, and the
// string-keyed maps that drive graph navigation. Calibrated against
// heap profiles on a 68k-vector index (50 dims, default M=16): live
// was ~408 MiB, i.e. ~6 KiB per vector overall. Using dims×4 + 5900 B
// keeps the formula honest as dims change (MiniLM at 384 would push
// the dims×4 term to 1.5 KiB per vector, overhead stays roughly flat).
func (v *VectorBackend) SizeBytes() uint64 {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.count == 0 {
		return 0
	}
	const hnswOverhead = 5900 // neighbor lists + map headers + priority-queue slack
	perVector := uint64(v.dims)*4 + hnswOverhead
	return uint64(v.count) * perVector
}
