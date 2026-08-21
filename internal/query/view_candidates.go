package query

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// ViewLayerSource is one generation of a composed view's stack as candidate
// enumeration sees it.
//
// The composed reader answers every node and edge question for the whole
// stack, but it cannot answer a full-text one: a text index is a corpus, each
// generation carries its own rows, and a handle only ever matches the
// generation it is pinned to. So a search over a composed view enumerates
// candidates from every corpus in the stack — the indexed corpus through the
// engine's ordinary search provider, and one of these per generation on top of
// it — and composes the results the same way the reader composes rows.
//
// Layer is not the corpus's twin, it is the corpus's ownership claim: which
// paths the generation replaced or deleted and which identities it speaks for.
// It is the contract the composed reader itself applies, so masking a lower
// corpus's hits asks the same question the reader asks of a lower row.
type ViewLayerSource struct {
	// Search enumerates candidates from this generation alone.
	Search search.Backend
	// Layer is what this generation hides from everything below it.
	Layer graph.OverlayLayerReader
}

// WithViewLayers returns a clone that reads through the composed view reader r
// and enumerates candidates across every generation stacked in it.
//
// An empty layer slice is the base request: the clone is exactly what
// WithReader returns, and every search it serves takes the base path with no
// per-generation work at all.
func (e *Engine) WithViewLayers(r graph.Reader, layers []ViewLayerSource) *Engine {
	clone := e.WithReader(r)
	if clone == nil || len(layers) == 0 {
		return clone
	}
	clone.viewLayers = layers
	return clone
}

// viewLayersActive reports whether this engine enumerates candidates across a
// composed view's stack rather than the base corpus alone.
func (e *Engine) viewLayersActive() bool { return len(e.viewLayers) > 0 }

// viewTextCandidates enumerates the text channel across the whole stack and
// returns one merged ranked list.
//
// base is what the engine's ordinary search provider returned for the indexed
// corpus; it enters the merge as the bottom source, and each generation in the
// stack is queried on its own handle above it. The three composition rules run
// before the merge: a candidate a higher generation speaks for is dropped, an
// identity two generations both returned is kept only from the higher one, and
// what survives is interleaved by rank position. Payload correctness is not
// this function's job — every surviving id is materialised through the
// composed reader by the caller, which also drops whatever the view hides that
// ownership alone did not catch.
func (e *Engine) viewTextCandidates(query string, limit int, base []search.SearchResult) []search.SearchResult {
	sources := make([][]search.SearchResult, 0, len(e.viewLayers)+1)
	sources = append(sources, base)
	for _, layer := range e.viewLayers {
		if layer.Search == nil {
			sources = append(sources, nil)
			continue
		}
		sources = append(sources, layer.Search.Search(query, limit))
	}
	e.composeViewSources(sources)
	return MergeRankedSources(sources, func(r search.SearchResult) string { return r.ID })
}

// composeViewSources applies masking and cross-source dedup to the per-source
// hit lists, rewriting each source in place with what survives.
//
// Masking is the reader's own ownership predicate: a generation speaks for an
// identity when it claims the identity's file — under either ownership mode,
// so a deletion hides the row as surely as a replacement — or when it carries
// or tombstoned the identity itself. A hit from a corpus below a generation
// that speaks for its identity is a row the view does not expose, and it is
// dropped before it can cost a candidate slot or a rank position.
//
// Dedup then keeps one occurrence per identity, from the highest source that
// returned it. In practice masking has already settled it — a generation whose
// corpus returned an id necessarily carries a node under that id, so the lower
// occurrence is masked — but the rule is stated here rather than inferred,
// because it is what makes the merge below order-independent.
func (e *Engine) composeViewSources(sources [][]search.SearchResult) {
	for i := range sources {
		if len(sources[i]) == 0 {
			continue
		}
		kept := make([]search.SearchResult, 0, len(sources[i]))
		for _, hit := range sources[i] {
			if hit.ID == "" || e.hiddenAboveSource(i, hit.ID) {
				continue
			}
			kept = append(kept, hit)
		}
		sources[i] = kept
	}
	// Highest source first, so the first occurrence of an id is the one to
	// keep and every lower repeat of it is dropped.
	seen := make(map[string]struct{})
	for i := len(sources) - 1; i >= 0; i-- {
		kept := make([]search.SearchResult, 0, len(sources[i]))
		for _, hit := range sources[i] {
			if _, dup := seen[hit.ID]; dup {
				continue
			}
			seen[hit.ID] = struct{}{}
			kept = append(kept, hit)
		}
		sources[i] = kept
	}
}

// hiddenAboveSource reports whether any generation above source speaks for an
// identity. Source 0 is the indexed corpus and source k>0 is viewLayers[k-1],
// so the generations above source k are viewLayers[k:].
func (e *Engine) hiddenAboveSource(source int, id string) bool {
	for i := source; i < len(e.viewLayers); i++ {
		layer := e.viewLayers[i].Layer
		if layer == nil {
			continue
		}
		if layer.CoversNodeID(id) || layer.OwnsNodeIdentity(id) {
			return true
		}
	}
	return false
}

// MergeRankedSources interleaves per-source ranked lists into one order.
//
// # The ranking policy, and why it is this one
//
// Each source is a different corpus. A generation's BM25 score is computed
// against the handful of files that generation re-derived, the indexed
// corpus's against a whole repository — the same document scores differently
// in each, and the difference is a property of the corpus statistics, not of
// the document's relevance. Comparing the two numbers, or rescaling one onto
// the other, would be inventing a comparison the data does not support.
//
// So the scores are not compared at all. What is compared is each hit's rank
// position inside its own source, which is the one quantity every corpus
// expresses on the same scale: "the best answer this corpus has" means the
// same thing whether the corpus holds four files or forty thousand. The
// sources are interleaved by that position, and a tie at one position is
// resolved toward the higher layer — the layer nearer what the caller is
// actually reading, and the only one that can carry content nothing below it
// has.
//
// This is an interim policy. The successor is exact per-view corpus
// statistics: with the document frequencies of the composed view rather than
// of each generation separately, every hit can be scored against one corpus
// and the merge becomes an ordinary sort. Until those statistics exist, rank
// position is the honest merge.
//
// id extracts the identity a duplicate is recognised by; pass nil, or return
// "", to keep every entry. Ranking within one source is preserved, so a source
// whose entries all outrank another's still comes out ahead of it.
func MergeRankedSources[T any](sources [][]T, id func(T) string) []T {
	total := 0
	for _, s := range sources {
		total += len(s)
	}
	if total == 0 {
		return nil
	}
	type entry struct {
		item   T
		rank   int
		source int
	}
	entries := make([]entry, 0, total)
	for si, s := range sources {
		for rank, item := range s {
			entries = append(entries, entry{item: item, rank: rank, source: si})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].rank != entries[j].rank {
			return entries[i].rank < entries[j].rank
		}
		return entries[i].source > entries[j].source
	})
	out := make([]T, 0, total)
	var seen map[string]struct{}
	if id != nil {
		seen = make(map[string]struct{}, total)
	}
	for _, e := range entries {
		if id != nil {
			if key := id(e.item); key != "" {
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
			}
		}
		out = append(out, e.item)
	}
	return out
}
