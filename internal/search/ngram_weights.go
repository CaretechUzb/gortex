package search

import (
	"sort"
	"unicode"

	"github.com/zzet/gortex/internal/graph"
)

// NgramTable is a per-repository, LLM-free table of learned sub-word
// boundary weights mined from the symbol names in the graph. Where the
// fixed character n-gram stage cuts every token at a fixed width, this
// table cuts each token at *high-information* boundaries — positions
// where the adjacent character pair is rare across the repo's symbol
// vocabulary, which is where a name tends to seam (the "tk" in
// "validateTokenizer" is far rarer than the "to"/"ke"/"en" inside
// "token", so the split lands at the seam, not mid-word).
//
// Go has no compile-time table generation, so this is NOT a comptime
// literal map: it is computed at index / analysis time, once per
// RunAnalysis pass, exactly like the auto-concept vocabulary. The build
// is one tokenizing pass over node names plus a bounded character-pair
// count, cheap enough to recompute on every reindex.
//
// The table feeds the sparse-ngram tokenizer (see ExpandSparseNgrams):
// when a non-empty table is installed on the backend, the tokenizer
// asks it where to Split each word token instead of slicing at a fixed
// n. A nil or empty table degrades the tokenizer to fixed character
// n-grams, so the search path is identical whether or not the table has
// been built yet.
type NgramTable struct {
	// boundaryPairs holds the character bigrams whose normalized
	// corpus frequency is low enough to count as a split seam. A pair
	// is packed as (hi<<16 | lo) of its two runes; only pairs over the
	// ASCII-letter alphabet are tracked, which covers essentially all
	// real symbol names. Presence in the set means "split between these
	// two characters". The set is derived deterministically from the
	// sorted frequency table at build time and never mutated after.
	boundaryPairs map[uint32]struct{}
}

// Ngram boundary-mining bounds. Mirrors the auto-concept caps in spirit
// — keep the pair count and the boundary set bounded on a large
// monorepo, and require enough evidence before trusting a seam.
const (
	// ngramMinTokenChars is the shortest token the boundary miner will
	// split. A token this short or shorter has no interior seam worth
	// learning; the tokenizer keeps it whole (or, in fixed mode, emits
	// its single full-length gram).
	ngramMinTokenChars = 5
	// ngramMinCorpusPairs is the minimum number of distinct adjacent
	// character pairs the corpus must yield before the table trusts its
	// frequency distribution. Below this the sample is too thin to tell
	// a rare seam from noise, and the table stays empty so the
	// tokenizer falls back to fixed n-grams.
	ngramMinCorpusPairs = 8
	// ngramBoundaryPercentile selects the rarest adjacent pairs as
	// seams: a pair counts as a boundary when its frequency rank falls
	// in the bottom this-many percent of all observed pairs. Lower =
	// fewer, higher-confidence seams.
	ngramBoundaryPercentile = 25
)

// BuildNgramBoundaries mines the per-repo sub-word boundary table from a
// graph. Only named code symbols (the same kinds auto-concept mining
// uses) contribute their names. A nil or empty graph — or one whose
// symbol names yield too few distinct character pairs to be
// trustworthy — yields an empty, safe-to-query table that reports
// Empty() == true, so the tokenizer degrades to fixed character
// n-grams.
//
// The build is deterministic: every map is drained into a sorted slice
// before any threshold is derived or any boundary is selected, so two
// builds over the same graph produce byte-identical boundary sets.
func BuildNgramBoundaries(g graph.Reader) *NgramTable {
	t := &NgramTable{boundaryPairs: map[uint32]struct{}{}}
	if g == nil {
		return t
	}

	// Pass 1: count how often each adjacent character pair occurs
	// inside the repo's symbol-name word tokens. We reuse the
	// auto-concept tokenizer so the boundary table is learned over the
	// same word vocabulary the rest of search tokenizes on.
	pairCount := map[uint32]int{}
	for _, n := range g.AllNodes() {
		if !autoConceptEligible(n.Kind) {
			continue
		}
		for _, tok := range autoConceptTokens(n.Name) {
			countAdjacentPairs(tok, pairCount)
		}
	}

	// Too thin a sample: keep the table empty rather than learn noise.
	if len(pairCount) < ngramMinCorpusPairs {
		return t
	}

	// Pass 2: rank the pairs by frequency, ascending, with a stable
	// tie-break on the packed key so the ordering — and therefore the
	// percentile cut — is identical across runs regardless of Go's
	// random map iteration. The rarest pairs are the high-information
	// seams.
	type pairFreq struct {
		key   uint32
		count int
	}
	ranked := make([]pairFreq, 0, len(pairCount))
	for k, c := range pairCount {
		ranked = append(ranked, pairFreq{k, c})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].count != ranked[j].count {
			return ranked[i].count < ranked[j].count
		}
		return ranked[i].key < ranked[j].key
	})

	// Select the bottom percentile as boundaries. At least one seam is
	// kept whenever the sample cleared the minimum, so a small but
	// valid corpus still learns something.
	cut := (len(ranked) * ngramBoundaryPercentile) / 100
	if cut < 1 {
		cut = 1
	}
	for _, pf := range ranked[:cut] {
		t.boundaryPairs[pf.key] = struct{}{}
	}
	return t
}

// InstallNgramBoundaries walks a search backend down to the BM25 layer
// and installs the learned boundary table on it, so the sparse-ngram
// tokenizer's split decisions become data-driven. The production
// backend is a Swappable wrapping either a HybridBackend (text+vector)
// or a bare BM25Backend; this unwraps both. Backends with no BM25 layer
// (Bleve, SymbolSearcher) do not run the sparse-ngram stage, so there
// is nothing to install and the call is a harmless no-op returning
// false.
//
// Symmetry contract: install the table before the backend is populated
// and leave it stable for the backend's lifetime. The index and query
// paths both read the installed table per call, so a single stable
// table keeps n-grammed postings and n-grammed query terms in lockstep.
// Installing a different table after postings exist would desynchronise
// them while the gate is on — callers re-install only as part of a
// fresh (re)index, never against a live, already-populated index.
func InstallNgramBoundaries(backend Backend, table NgramBoundaries) bool {
	bm := bm25Of(backend)
	if bm == nil {
		return false
	}
	bm.SetNgramBoundaries(table)
	return true
}

// BuildAndInstallNgramBoundaries mines and installs a boundary table only
// when backend actually contains a BM25 layer. Capability detection must
// precede BuildNgramBoundaries: native SQLite FTS and Bleve never consume the
// table, and walking the whole graph for them is pure allocation and I/O.
func BuildAndInstallNgramBoundaries(backend Backend, g graph.Reader) bool {
	bm := bm25Of(backend)
	if bm == nil {
		return false
	}
	bm.SetNgramBoundaries(BuildNgramBoundaries(g))
	return true
}

// bm25Of unwraps a backend down to its *BM25Backend, or returns nil
// when the backend has no BM25 layer. Mirrors the unwrap chain the
// engine uses for the bundle fast path: Swappable → HybridBackend →
// BM25Backend.
func bm25Of(backend Backend) *BM25Backend {
	switch b := backend.(type) {
	case *BM25Backend:
		return b
	case *Swappable:
		return bm25Of(b.Inner())
	case *HybridBackend:
		return bm25Of(b.TextBackend())
	default:
		return nil
	}
}

// countAdjacentPairs tallies every adjacent ASCII-letter character pair
// in one lowercase token into counts. Pairs touching a non-ASCII or
// non-letter rune are skipped — digits and symbols are not part of the
// learned alphabet, mirroring how the FTS stemmer leaves digit-bearing
// tokens alone. The token is assumed already lowercased by
// autoConceptTokens.
func countAdjacentPairs(tok string, counts map[uint32]int) {
	r := []rune(tok)
	for i := 1; i < len(r); i++ {
		a, b := r[i-1], r[i]
		if !isLearnableRune(a) || !isLearnableRune(b) {
			continue
		}
		counts[packPair(a, b)]++
	}
}

// isLearnableRune reports whether a rune participates in the learned
// pair alphabet: ASCII letters only.
func isLearnableRune(r rune) bool {
	return r <= unicode.MaxASCII && unicode.IsLetter(r)
}

// packPair packs two runes into a single uint32 key (hi<<16 | lo).
// Both runes are ASCII here, so they fit in 16 bits each with room to
// spare.
func packPair(a, b rune) uint32 {
	return uint32(a)<<16 | uint32(b)
}

// Empty reports whether the table learned any boundaries. A nil table,
// or one mined from an empty / too-thin graph, is empty — callers MUST
// treat an empty table as "no learned boundaries" and degrade to fixed
// behaviour rather than splitting on nothing. Nil-safe so a typed-nil
// *NgramTable stored in the NgramBoundaries interface still answers
// correctly.
func (t *NgramTable) Empty() bool {
	return t == nil || len(t.boundaryPairs) == 0
}

// BoundaryCount reports the number of learned seam pairs. Used by tests
// and diagnostics.
func (t *NgramTable) BoundaryCount() int {
	if t == nil {
		return 0
	}
	return len(t.boundaryPairs)
}

// Split cuts a token's runes at the learned high-information boundaries
// and returns the resulting segments left-to-right. A split is taken
// between positions i-1 and i when the adjacent character pair is a
// learned seam. The token is never split so finely that a segment falls
// below the minimum gram length: a candidate seam that would strand a
// sub-minimal segment on either side is skipped, so Split always yields
// segments the tokenizer can use. When the table is empty Split returns
// the whole token as a single segment, leaving the fixed-n fallback to
// the caller.
//
// Split is deterministic — it scans left to right and consults only the
// immutable boundary set — and never mutates the input.
func (t *NgramTable) Split(runes []rune) []string {
	if t.Empty() || len(runes) < ngramMinTokenChars {
		return []string{string(runes)}
	}

	var (
		segs  []string
		start int
	)
	for i := 1; i < len(runes); i++ {
		a, b := runes[i-1], runes[i]
		if !isLearnableRune(a) || !isLearnableRune(b) {
			continue
		}
		if _, seam := t.boundaryPairs[packPair(a, b)]; !seam {
			continue
		}
		// Only take the seam if both the segment it closes and the
		// remainder it opens can still carry a usable gram — guards
		// against shredding the token into sub-minimal fragments.
		left := i - start
		right := len(runes) - i
		if left < sparseNgramMinN || right < sparseNgramMinN {
			continue
		}
		segs = append(segs, string(runes[start:i]))
		start = i
	}
	segs = append(segs, string(runes[start:]))
	return segs
}
