package indexer

import (
	"regexp"
	"regexp/syntax"
	"sort"

	"github.com/zzet/gortex/internal/search/trigram"
)

// GrepText runs a trigram-accelerated literal search for query across
// the indexed repo, returning up to limit matching lines (a
// non-positive limit returns every match). The trigram index is built
// lazily on first use and reused across calls; it is rebuilt only when
// a full or incremental index has advanced the repo generation, so a
// burst of searches between reindexes all hit a warm index.
func (idx *Indexer) GrepText(query string, limit int) []trigram.Match {
	if query == "" {
		return nil
	}
	s := idx.warmTrigramSearcher()
	if s == nil {
		return nil
	}
	return s.Grep(query, limit)
}

// warmTrigramSearcher returns the current trigram searcher, rebuilding it
// when the index generation has moved since the cached searcher was
// built. Returns nil before anything has been indexed.
func (idx *Indexer) warmTrigramSearcher() *trigram.Searcher {
	gen := idx.indexGen.Load()

	idx.trigramMu.Lock()
	if idx.trigramSearcher == nil || idx.trigramGen != gen {
		root := idx.rootPath
		if root != "" {
			idx.mtimeMu.RLock()
			rels := make([]string, 0, len(idx.fileMtimes))
			for rel := range idx.fileMtimes {
				rels = append(rels, rel)
			}
			idx.mtimeMu.RUnlock()
			sort.Strings(rels)

			idx.trigramSearcher = trigram.Build(root, rels)
			idx.trigramGen = gen
		}
	}
	searcher := idx.trigramSearcher
	var release func()
	if searcher != nil {
		release = idx.trigramReleaseLocked()
	}
	idx.trigramMu.Unlock()

	// Touch only after dropping trigramMu: an over-budget touch may release a
	// different repo, whose callback takes that repo's trigramMu. The lease
	// captured above prevents a delayed callback from racing a later re-touch.
	if release != nil {
		idx.trigramBudget().touch(idx, release)
	}
	return searcher
}

// trigramBudget returns the budget this Indexer participates in, defaulting
// to the process-wide one so an Indexer built without wiring is still bounded.
func (idx *Indexer) trigramBudget() *trigramBudget {
	if idx.trigramBudgetOverride != nil {
		return idx.trigramBudgetOverride
	}
	return processTrigramBudget
}

// releaseTrigramSearcher drops this repo's built trigram index. Called by the
// budget when the repo has gone idle or has been pushed out by more recently
// used repos. Safe to call when nothing is built; the next search rebuilds
// from disk, which costs latency but never correctness — the searcher is only
// a candidate filter over files Grep re-reads anyway.
func (idx *Indexer) releaseTrigramSearcher() {
	if idx == nil {
		return
	}
	idx.trigramMu.Lock()
	idx.trigramLease++
	idx.trigramSearcher = nil
	idx.trigramGen = 0
	idx.trigramMu.Unlock()
}

// trigramReleaseLocked returns a callback tied to the current cache-use lease.
// The caller must hold trigramMu. A re-touch advances the lease before the old
// callback can take the mutex, making a delayed timer/LRU release a no-op.
func (idx *Indexer) trigramReleaseLocked() func() {
	idx.trigramLease++
	lease := idx.trigramLease
	return func() { idx.releaseTrigramSearcherLease(lease) }
}

func (idx *Indexer) releaseTrigramSearcherLease(lease uint64) {
	idx.trigramMu.Lock()
	if idx.trigramLease == lease {
		idx.trigramSearcher = nil
		idx.trigramGen = 0
	}
	idx.trigramMu.Unlock()
}

// GrepRegexp runs a trigram-accelerated regular-expression search for
// pattern across the indexed repo, returning up to limit matching lines
// (a non-positive limit returns every match). The pattern's mandatory
// literal runs are extracted and used to pre-filter the candidate file
// set via the trigram index; the compiled regexp then verifies each
// candidate line. pathPrefix, when non-empty, restricts the scan to
// files under that forward-slash repo-relative prefix.
//
// A pattern that does not compile returns an error so the caller can
// surface it; an empty pattern returns no matches and no error.
func (idx *Indexer) GrepRegexp(pattern, pathPrefix string, limit int) ([]trigram.Match, error) {
	if pattern == "" {
		return nil, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	s := idx.warmTrigramSearcher()
	if s == nil {
		return nil, nil
	}
	literals := extractRegexLiterals(pattern)
	return s.GrepRegexp(re, literals, pathPrefix, limit), nil
}

// extractRegexLiterals returns the literal substrings the regexp must
// contain on every match. It parses the pattern with regexp/syntax and
// walks the syntax tree for OpLiteral runs that are mandatory — i.e.
// reached through OpConcat / OpCapture only, never under an alternation
// or a zero-min repetition. Each such run is rendered to its UTF-8
// bytes; runs shorter than three bytes are dropped (they cannot be
// trigram-filtered). A pattern that does not parse yields no literals,
// which is safe: the caller falls back to scanning every file.
func extractRegexLiterals(pattern string) []string {
	reSyn, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil
	}
	var out []string
	var walk func(re *syntax.Regexp)
	walk = func(re *syntax.Regexp) {
		switch re.Op {
		case syntax.OpLiteral:
			if s := string(re.Rune); len(s) >= 3 {
				out = append(out, s)
			}
		case syntax.OpConcat, syntax.OpCapture:
			// A concatenation / capture preserves the mandatory nature
			// of each child, so recurse into all of them.
			for _, sub := range re.Sub {
				walk(sub)
			}
		case syntax.OpPlus:
			// x+ guarantees at least one x — its literal is mandatory.
			for _, sub := range re.Sub {
				walk(sub)
			}
			// Other ops (OpAlternate, OpStar, OpQuest, OpRepeat with
			// Min==0, character classes, anchors) do not contribute a
			// guaranteed literal — stop descending.
		}
	}
	walk(reSyn)
	return out
}
