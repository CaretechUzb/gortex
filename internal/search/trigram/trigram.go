// Package trigram is a Zoekt-style trigram index over file content —
// an alternative grep backbone. Each document's content contributes
// its set of 3-byte substrings (trigrams) to a posting map; a literal
// query intersects the posting lists of its own trigrams down to a
// small candidate set, which a literal scan then verifies. It turns a
// repo-wide substring search from an O(total bytes) walk into an
// O(candidate bytes) one.
package trigram

import (
	"slices"
	"sync"
)

// trigram is three consecutive content bytes packed into a uint32.
type trigram uint32

// Index maps each trigram to the document IDs whose content contains
// it. It is the candidate-filter half of the search; verification of
// an actual match is the caller's job. Safe for concurrent use.
type Index struct {
	mu     sync.RWMutex
	post   map[trigram][]uint32 // trigram -> docIDs containing it
	perDoc map[uint32][]trigram // docID -> its trigrams, for O(doc) removal
	docs   map[uint32]struct{}  // known docIDs
	// pairs counts distinct (document, trigram) pairs. It is the size
	// driver of the whole index — every pair is one posting and one
	// per-document trigram — and is maintained incrementally so a memory
	// budget can size the index in O(1) instead of walking the maps.
	pairs int64
}

// New returns an empty index.
func New() *Index {
	return &Index{
		post:   make(map[trigram][]uint32),
		perDoc: make(map[uint32][]trigram),
		docs:   make(map[uint32]struct{}),
	}
}

// trigrams returns the distinct trigrams of content. Content shorter
// than three bytes has none.
func trigrams(content []byte) []trigram {
	if len(content) < 3 {
		return nil
	}
	seen := make(map[trigram]struct{})
	for i := 0; i+2 < len(content); i++ {
		tg := trigram(content[i])<<16 | trigram(content[i+1])<<8 | trigram(content[i+2])
		seen[tg] = struct{}{}
	}
	out := make([]trigram, 0, len(seen))
	for tg := range seen {
		out = append(out, tg)
	}
	return out
}

// Add records that document docID's content contains its trigrams.
// Re-adding an existing docID first drops its previous postings, so
// Add doubles as the re-index path for a changed file.
func (ix *Index) Add(docID uint32, content []byte) {
	tgs := trigrams(content)
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.removeLocked(docID)
	ix.docs[docID] = struct{}{}
	for _, tg := range tgs {
		ix.post[tg] = append(ix.post[tg], docID)
	}
	ix.perDoc[docID] = tgs
	ix.pairs += int64(len(tgs))
}

// Remove drops docID from every posting list it appears in.
func (ix *Index) Remove(docID uint32) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	ix.removeLocked(docID)
}

func (ix *Index) removeLocked(docID uint32) {
	tgs, ok := ix.perDoc[docID]
	if !ok {
		return
	}
	for _, tg := range tgs {
		list := ix.post[tg]
		for i, d := range list {
			if d == docID {
				ix.post[tg] = append(list[:i], list[i+1:]...)
				break
			}
		}
		if len(ix.post[tg]) == 0 {
			delete(ix.post, tg)
		}
	}
	ix.pairs -= int64(len(tgs))
	delete(ix.perDoc, docID)
	delete(ix.docs, docID)
}

// approxBytes estimates the heap this index retains.
//
// Every distinct (document, trigram) pair costs one posting and one entry
// in that document's trigram slice; every distinct trigram in the corpus
// additionally costs one slot in the posting map, which is the dominant
// term for high-entropy content. Go's append growth leaves slack in both
// slices, charged here as a flat multiplier.
//
// Calibrated against measured heap: a single 2.02 MiB PNG (1,729,086
// pairs) estimates 98.9 MiB against 101.14 MiB measured. The estimate
// errs high for very small indexes, where map bucket granularity
// dominates — the safe direction for a budget.
func (ix *Index) approxBytes() int64 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	const (
		postMapSlotBytes   = 48 // one map[trigram][]uint32 slot at Go's load factor
		perDocMapSlotBytes = 48 // one map[uint32][]trigram slot
		docSlotBytes       = 16 // one map[uint32]struct{} slot
		pairBytes          = 12 // 4-byte posting + 4-byte trigram, 1.5x append slack
	)
	return int64(len(ix.post))*postMapSlotBytes +
		int64(len(ix.perDoc))*perDocMapSlotBytes +
		int64(len(ix.docs))*docSlotBytes +
		ix.pairs*pairBytes
}

// Candidates returns the docIDs that could contain query: every
// document whose trigram set includes all of the query's trigrams.
// A query shorter than three bytes cannot be trigram-filtered, so
// every known document is returned — the caller verifies regardless.
// The result is sorted ascending.
func (ix *Index) Candidates(query string) []uint32 {
	qtgs := trigrams([]byte(query))
	ix.mu.RLock()
	defer ix.mu.RUnlock()

	if len(qtgs) == 0 {
		out := make([]uint32, 0, len(ix.docs))
		for d := range ix.docs {
			out = append(out, d)
		}
		slices.Sort(out)
		return out
	}

	// A document is a candidate iff it appears under every one of the
	// query's distinct trigrams. Counting appearances avoids needing
	// the posting lists pre-sorted for an intersection.
	hits := make(map[uint32]int)
	for _, tg := range qtgs {
		for _, d := range ix.post[tg] {
			hits[d]++
		}
	}
	out := make([]uint32, 0)
	for d, c := range hits {
		if c == len(qtgs) {
			out = append(out, d)
		}
	}
	slices.Sort(out)
	return out
}

// docIDs returns every indexed docID, ascending. It is the candidate set
// for a query with no usable trigram, and is derived rather than cached so
// an incremental Add / Remove cannot leave it stale.
func (ix *Index) docIDs() []uint32 {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]uint32, 0, len(ix.docs))
	for d := range ix.docs {
		out = append(out, d)
	}
	slices.Sort(out)
	return out
}

// DocCount returns the number of indexed documents.
func (ix *Index) DocCount() int {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return len(ix.docs)
}
