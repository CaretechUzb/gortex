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
// When a delegate is installed, the in-process HNSW is absent: Add tracks
// compatibility-path writes without allocating, and Search forwards to the
// delegate's SimilarTo. Durable hits carry their parent IDs, so chunk results
// can be collapsed without retaining a process-local chunk map.
type VectorBackend struct {
	graph *hnsw.Graph[string]
	count int
	dims  int
	// chunkMap maps a synthetic chunk vector ID ("<symbolID>#chunkK")
	// to its parent symbol ID. It is non-empty only for the legacy
	// in-process build path; durable delegates return ParentID with each hit.
	chunkMap map[string]string
	mu       sync.RWMutex

	// delegate is the optional durable vector searcher. Delegated backends
	// never retain an in-process HNSW graph. delegateCount is the complete
	// durable corpus size; delegateChunkCount is the number of vectors that
	// represent chunks rather than whole symbols.
	delegate           VectorDelegate
	delegateCount      int
	delegateChunkCount int
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

// NewDelegatedVector creates a vector backend that routes searches to a
// durable store without allocating an in-process HNSW graph. vectorCount and
// chunkCount describe the complete durable corpus, not just this process's
// writes, so warm-started hybrid search is enabled immediately.
func NewDelegatedVector(dims int, delegate VectorDelegate, vectorCount, chunkCount int) *VectorBackend {
	if delegate == nil {
		panic("search.NewDelegatedVector: nil delegate")
	}
	if dims <= 0 {
		panic("search.NewDelegatedVector: non-positive dimensions")
	}
	if vectorCount < 0 {
		panic("search.NewDelegatedVector: negative vector count")
	}
	if chunkCount < 0 || chunkCount > vectorCount {
		panic("search.NewDelegatedVector: invalid chunk count")
	}
	return &VectorBackend{
		dims:               dims,
		delegate:           delegate,
		delegateCount:      vectorCount,
		delegateChunkCount: chunkCount,
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
	return v.delegateChunkCount > 0 || len(v.chunkMap) > 0
}

// Add indexes a symbol with its embedding vector.
func (v *VectorBackend) Add(id string, vector []float32) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.delegate != nil {
		// Legacy compatibility for callers that install a delegate before
		// pushing vectors themselves. New publication must construct the
		// backend with NewDelegatedVector and complete durable statistics.
		// Either way Add never allocates an in-process HNSW in delegated mode.
		v.delegateCount++
		return
	}
	v.graph.Add(hnsw.Node[string]{
		Key:   id,
		Value: hnsw.Vector(vector),
	})
	v.count++
}

// SetDelegate is the legacy compatibility switch for callers that select a
// delegate before writing their corpus. It releases all prior heap and chunk
// state atomically; subsequent Add calls only advance this process's accepted
// write count. New and warm-start publication must use NewDelegatedVector so
// Count and HasChunks describe the complete durable corpus immediately.
func (v *VectorBackend) SetDelegate(d VectorDelegate) {
	v.mu.Lock()
	defer v.mu.Unlock()

	// Switching modes must also release the old heap index. Merely routing
	// reads to d would leave the complete HNSW graph, its count, and chunk
	// ownership map retained for the lifetime of the daemon.
	v.graph = nil
	v.count = 0
	v.chunkMap = nil
	v.delegate = d
	v.delegateCount = 0
	v.delegateChunkCount = 0
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
		ids := make([]string, 0, len(hits))
		seen := make(map[string]struct{}, len(hits))
		for _, h := range hits {
			id := h.NodeID
			if h.ParentID != "" {
				id = h.ParentID
			}
			if id == "" {
				continue
			}
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
		return ids
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.count == 0 || v.graph == nil {
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
	if v.delegate != nil || v.count == 0 || v.graph == nil {
		return 0
	}
	const hnswOverhead = 5900 // neighbor lists + map headers + priority-queue slack
	perVector := uint64(v.dims)*4 + hnswOverhead
	return uint64(v.count) * perVector
}

// Close releases all process-local vector state. A delegate is externally
// owned (normally by the durable graph store), so it is detached but never
// closed here.
func (v *VectorBackend) Close() {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.graph = nil
	v.count = 0
	v.chunkMap = nil
	v.delegate = nil
	v.delegateCount = 0
	v.delegateChunkCount = 0
}
