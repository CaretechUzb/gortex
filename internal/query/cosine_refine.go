package query

import (
	"context"
	"math"
	"sort"

	"github.com/zzet/gortex/internal/embedding"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/search/rerank"
)

// defaultCosineTopN bounds how many of the top ranked candidates the
// post-rerank cosine refinement re-scores. The stage embeds the query
// once and fetches this many stored vectors in one batch — keeping the
// bound small (a few dozen) means the refinement is a cheap O(topN)
// pass over an already-ranked head, never a re-rank of the whole pool.
const defaultCosineTopN = 32

// embedderProvider is the optional capability a search backend exposes
// when it carries a query embedder (the HybridBackend). Declared here
// so the query package can recover the embedder from whatever backend
// the engine currently holds without depending on the concrete type.
type embedderProvider interface {
	Embedder() embedding.Provider
}

// backendEmbedder extracts the query embedder from a search backend,
// through the backend's lock-scoped capability surface. Returns nil when no
// embedder is reachable — the caller treats that as "vector channel inactive"
// and skips the refinement entirely.
func backendEmbedder(b search.Backend) embedding.Provider {
	if b == nil {
		return nil
	}
	if ep, ok := b.(embedderProvider); ok {
		if e := ep.Embedder(); e != nil {
			return e
		}
	}
	return nil
}

// refineByCosine re-orders the top of an already-ranked candidate slice
// by exact cosine similarity between the query embedding and each
// candidate's stored embedding — recovering the precise semantic
// distance the rank-based SemanticSignal throws away.
//
// It is deliberately best-effort and regression-safe: it is a no-op
// (returning cands untouched) whenever the vector channel is inactive,
// the store can't read embeddings back, the embedder is absent, or the
// query fails to embed. Only candidates whose stored vector matches the
// query embedding's dimension participate; a candidate with no stored
// vector keeps its rerank position and is never demoted below one that
// was scored.
//
// Only the top `topN` candidates are touched. The tail below topN keeps
// its rerank order, so the refinement can sharpen the head without
// disturbing the long fallback tail. The relative order of refined
// candidates among themselves is decided purely by cosine; ties fall
// back to the incoming rerank order for determinism.
func refineByCosine(
	query string,
	cands []*rerank.Candidate,
	embedder embedding.Provider,
	vectors graph.VectorSearcher,
	topN int,
) []*rerank.Candidate {
	if embedder == nil || vectors == nil || query == "" || len(cands) < 2 {
		return cands
	}
	if topN <= 0 {
		topN = defaultCosineTopN
	}
	head := topN
	if head > len(cands) {
		head = len(cands)
	}

	// Collect the candidate IDs in the head window and pull their
	// stored vectors in one batch. An empty result means none of the
	// head candidates were embedded — nothing to refine.
	ids := make([]string, 0, head)
	for _, c := range cands[:head] {
		if c != nil && c.Node != nil && c.Node.ID != "" {
			ids = append(ids, c.Node.ID)
		}
	}
	if len(ids) == 0 {
		return cands
	}
	stored := vectors.GetEmbeddings(ids)
	if len(stored) == 0 {
		return cands
	}

	// Embed the query exactly once. A failure here is not an error to
	// the caller — search must still return the rerank order.
	qVec, err := embedder.Embed(context.Background(), query)
	if err != nil || len(qVec) == 0 {
		return cands
	}
	qNorm := vecNorm(qVec)
	if qNorm == 0 {
		return cands
	}

	// Score every head candidate that has a dimension-matched stored
	// vector. scored[i] is the cosine similarity (higher = closer) for
	// cands[i]; candidates without a usable vector are left unscored
	// and keep their incoming order relative to one another.
	type scoredCand struct {
		cand   *rerank.Candidate
		cosine float64
		scored bool
		order  int // incoming rerank position, the stable tiebreak
	}
	window := make([]scoredCand, head)
	anyScored := false
	for i, c := range cands[:head] {
		sc := scoredCand{cand: c, order: i}
		if c != nil && c.Node != nil {
			if vec, ok := stored[c.Node.ID]; ok && len(vec) == len(qVec) {
				if cNorm := vecNorm(vec); cNorm > 0 {
					sc.cosine = cosineSimilarity(qVec, vec, qNorm, cNorm)
					sc.scored = true
					anyScored = true
				}
			}
		}
		window[i] = sc
	}
	if !anyScored {
		return cands
	}

	// Stable sort: scored candidates ahead of unscored ones, scored
	// ranked by descending cosine, and every tie (including the whole
	// unscored block) broken by the incoming rerank order so the result
	// is deterministic and an unscored candidate never leapfrogs a
	// scored one.
	sort.SliceStable(window, func(a, b int) bool {
		wa, wb := window[a], window[b]
		if wa.scored != wb.scored {
			return wa.scored // scored sorts before unscored
		}
		if wa.scored && wb.scored && wa.cosine != wb.cosine {
			return wa.cosine > wb.cosine
		}
		return wa.order < wb.order
	})

	out := make([]*rerank.Candidate, 0, len(cands))
	for _, w := range window {
		out = append(out, w.cand)
	}
	out = append(out, cands[head:]...)
	return out
}

// vecNorm returns the Euclidean (L2) norm of v as a float64.
func vecNorm(v []float32) float64 {
	var sum float64
	for _, f := range v {
		d := float64(f)
		sum += d * d
	}
	return math.Sqrt(sum)
}

// cosineSimilarity returns cosine_similarity(a, b) in [-1, 1] given
// precomputed norms; higher means more similar. a and b are assumed
// equal length with non-zero norms.
func cosineSimilarity(a, b []float32, aNorm, bNorm float64) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	sim := dot / (aNorm * bNorm)
	if sim > 1 {
		sim = 1
	} else if sim < -1 {
		sim = -1
	}
	return sim
}
