package trigram

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/zzet/gortex/internal/pathguard"
)

// Match is one line of one file that contains the query literal.
type Match struct {
	Path string `json:"path"` // forward-slash repo-relative path
	Line int    `json:"line"` // 1-based line number
	Text string `json:"text"` // the matching line
}

// Searcher is a trigram-accelerated literal code search over a fixed
// set of files. Build it once against a repo's file list, then Grep it
// repeatedly. It is safe for concurrent Grep calls.
type Searcher struct {
	root         string
	resolvedRoot string // root with symlinks evaluated, for confinement tests
	ix           *Index
	paths        []string // docID -> forward-slash repo-relative path

	// searchable is the ascending set of docIDs a query with no usable
	// trigram may scan. It is exactly the indexed set: binary, oversized
	// and unreadable documents hold no postings and are not searched.
	searchable []uint32
	// indexedBytes is the summed length of the content actually indexed.
	// It is the input to the caller's memory budget, not a search input.
	indexedBytes int64
}

// Build reads every file — forward-slash repo-relative paths under
// root — and indexes its content. A file that cannot be read is left
// unindexed (it never matches) but keeps its docID slot so the rest
// stay aligned.
//
// Two further classes are kept out, and like an unreadable file they
// never match:
//
//   - Binary content (IsBinary). A literal text query cannot meaningfully
//     match compressed or encoded bytes, and indexing them costs roughly
//     50x the file's own size — a single 2 MiB PNG measured 101 MiB of
//     index.
//   - Anything over maxIndexedBytes, a per-document sanity ceiling well
//     above any file a person greps.
//
// A path that is a symlink out of root is treated as unreadable. The
// indexer walk already refuses to admit one, so this is the second of two
// independent barriers: it keeps a corpus assembled by some other route
// (a stale on-disk file list, a caller building its own relPaths) from
// turning this searcher into an arbitrary-file-read primitive.
func Build(root string, relPaths []string) *Searcher {
	s := &Searcher{
		root:         root,
		resolvedRoot: pathguard.ResolveRoot(root),
		ix:           New(),
		paths:        make([]string, len(relPaths)),
	}
	for i, rel := range relPaths {
		rel = filepath.ToSlash(rel)
		s.paths[i] = rel
		abs := filepath.Join(root, filepath.FromSlash(rel))
		if pathguard.EscapesResolvedRoot(abs, s.resolvedRoot) {
			continue
		}
		// Stat first: an oversized file must not be read at all, which is
		// the whole point of the ceiling.
		info, err := os.Stat(abs)
		if err != nil || info.IsDir() || info.Size() > maxIndexedBytes {
			continue
		}
		content, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		if IsBinary(content) {
			continue
		}
		s.ix.Add(uint32(i), content)
		s.indexedBytes += int64(len(content))
		s.searchable = append(s.searchable, uint32(i))
	}
	slices.Sort(s.searchable)
	return s
}

// candidates returns the docIDs to verify for query.
func (s *Searcher) candidates(query string) []uint32 {
	return s.ix.Candidates(query)
}

// IndexedBytes reports the total content length the searcher actually
// indexed. Callers use it to size the searcher against a memory budget;
// it excludes binary and oversized documents, which hold no index state.
func (s *Searcher) IndexedBytes() int64 {
	if s == nil {
		return 0
	}
	return s.indexedBytes
}

// ApproxIndexBytes estimates the heap the index retains, for budgeting.
// The dominant terms are the posting map (one entry plus a one-element
// posting slice per distinct (doc, trigram) pair) and the per-document
// trigram slices; a path string rounds out each doc. The coefficients
// were fitted against measured heap for this repo's corpus and track it
// within about 10%.
func (s *Searcher) ApproxIndexBytes() int64 {
	if s == nil {
		return 0
	}
	return s.ix.approxBytes() + int64(len(s.paths))*40
}

// openConfined opens the indexed file at rel for scanning, refusing one
// whose real location escapes the searcher's root.
//
// Only the leaf is Lstat'd. That is sufficient rather than sloppy: an
// intermediate directory cannot be a symlink, because filepath.WalkDir
// never descends one, so no path in the corpus reaches through a linked
// directory. Checking the leaf costs one Lstat per scanned file; fully
// resolving every component would cost several per file on the hot search
// path and buy nothing given that provenance.
func (s *Searcher) openConfined(rel string) (*os.File, error) {
	abs := filepath.Join(s.root, filepath.FromSlash(rel))
	if pathguard.EscapesResolvedRoot(abs, s.resolvedRoot) {
		return nil, os.ErrPermission
	}
	return os.Open(abs)
}

// Grep returns up to limit lines, across the indexed files, that
// contain the literal query. The trigram index narrows the file set;
// each candidate file is then scanned to confirm the match and locate
// its lines. Results are ordered by file, then by line. A non-positive
// limit returns every match.
func (s *Searcher) Grep(query string, limit int) []Match {
	if query == "" {
		return nil
	}
	var matches []Match
	for _, docID := range s.candidates(query) {
		if int(docID) >= len(s.paths) {
			continue
		}
		rel := s.paths[docID]
		f, err := s.openConfined(rel)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			if strings.Contains(text, query) {
				matches = append(matches, Match{Path: rel, Line: line, Text: text})
				if limit > 0 && len(matches) >= limit {
					_ = f.Close()
					return matches
				}
			}
		}
		_ = f.Close()
	}
	return matches
}

// DocCount returns the number of indexed files.
func (s *Searcher) DocCount() int { return s.ix.DocCount() }

// GrepRegexp returns up to limit lines, across the indexed files, that
// the compiled regexp re matches. requiredLiterals is a set of literal
// substrings (each ideally >= 3 bytes) that every matching line's file
// must contain — they come from the regex's own mandatory literal runs
// and let the trigram index narrow the candidate file set. When
// requiredLiterals is empty no trigram pre-filter is possible and every
// indexed file is scanned. Results are ordered by file, then by line.
// A non-positive limit returns every match.
//
// pathPrefix, when non-empty, restricts the scan to files whose
// forward-slash repo-relative path starts with it.
func (s *Searcher) GrepRegexp(re *regexp.Regexp, requiredLiterals []string, pathPrefix string, limit int) []Match {
	if re == nil {
		return nil
	}

	// Build the candidate doc set. Each required literal intersects its
	// trigram posting list into the running set; the first literal
	// seeds it. With no usable literal we fall back to every doc.
	var candidates map[uint32]struct{}
	for _, lit := range requiredLiterals {
		if len(lit) < 3 {
			// Too short to trigram-filter — skip; the regex scan still
			// verifies, so correctness is unaffected.
			continue
		}
		got := make(map[uint32]struct{})
		for _, id := range s.ix.Candidates(lit) {
			got[id] = struct{}{}
		}
		if candidates == nil {
			candidates = got
			continue
		}
		for id := range candidates {
			if _, ok := got[id]; !ok {
				delete(candidates, id)
			}
		}
	}

	var docIDs []uint32
	if candidates == nil {
		// No usable literal to trigram-filter on: scan every searchable
		// document. Binary, oversized and unreadable documents are
		// excluded here exactly as they are from the literal path.
		docIDs = slices.Clone(s.searchable)
	} else {
		docIDs = make([]uint32, 0, len(candidates))
		for id := range candidates {
			docIDs = append(docIDs, id)
		}
		slices.Sort(docIDs)
	}

	var matches []Match
	for _, docID := range docIDs {
		if int(docID) >= len(s.paths) {
			continue
		}
		rel := s.paths[docID]
		if pathPrefix != "" && !strings.HasPrefix(rel, pathPrefix) {
			continue
		}
		f, err := s.openConfined(rel)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			if re.MatchString(text) {
				matches = append(matches, Match{Path: rel, Line: line, Text: text})
				if limit > 0 && len(matches) >= limit {
					_ = f.Close()
					return matches
				}
			}
		}
		_ = f.Close()
	}
	return matches
}
