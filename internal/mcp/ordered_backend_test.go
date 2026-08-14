package mcp

import (
	"strings"
	"sync"
	"unicode"

	"github.com/zzet/gortex/internal/search"
)

// orderedBackend is a search.Backend that answers with a literal,
// hand-written candidate ordering instead of a ranked one.
//
// The MCP fixtures that use it are not testing a ranker: they test
// handler-layer logic (post-filter escalation, soup split/merge,
// equivalence rewrite, over-fetch retention, name diversification)
// GIVEN some candidate ordering. Encoding that ordering directly makes
// each fixture's premise readable and stable, instead of leaving it to
// emerge from term frequencies in a hand-tuned corpus.
//
// Retrieval model, deliberately crude: the query is split on
// whitespace, each field lowercased and stripped of edge punctuation,
// and the ordered ID lists of every matching token are unioned in
// token order (first token's list first, deduped). A query whose
// tokens are all unknown falls back to the optional default list —
// so a fixture can either key retrieval on specific tokens (the
// rewrite is then the thing under test: a symbol is reachable only
// if the handler actually queried one of its tokens) or hand the same
// ordering to every query.
//
// Note the tokens are whole whitespace fields, NOT camelCase parts:
// "WidgetExtensions" is the single token "widgetextensions", so a
// fixture can give a compound name and one of its parts different
// orderings.
type orderedBackend struct {
	mu      sync.Mutex
	byToken map[string][]string // query token -> ordered node IDs
	def     []string            // answer when no token matched
	limits  []int               // every limit Search was called with
}

func newOrderedBackend() *orderedBackend {
	return &orderedBackend{byToken: make(map[string][]string)}
}

// put appends ids to the ordering token answers with. Call order is
// the ranking: the first ID put is the top hit.
func (o *orderedBackend) put(token string, ids ...string) {
	key := strings.ToLower(token)
	o.byToken[key] = append(o.byToken[key], ids...)
}

// putDefault appends ids to the ordering returned for a query whose
// tokens are all unknown.
func (o *orderedBackend) putDefault(ids ...string) {
	o.def = append(o.def, ids...)
}

// searchLimits returns every limit the engine asked this backend for,
// oldest first.
func (o *orderedBackend) searchLimits() []int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]int(nil), o.limits...)
}

// Add / Remove are no-ops: the ordering is declared by the fixture, not
// accumulated from indexed text.
func (o *orderedBackend) Add(string, ...string) {}
func (o *orderedBackend) Remove(string)         {}
func (o *orderedBackend) Close()                {}

// Count reports the number of distinct IDs the backend can return. It
// must be positive for a populated fixture: the engine routes to its
// substring fallback when the backend reports an empty corpus.
func (o *orderedBackend) Count() int {
	seen := make(map[string]bool)
	for _, ids := range o.byToken {
		for _, id := range ids {
			seen[id] = true
		}
	}
	for _, id := range o.def {
		seen[id] = true
	}
	return len(seen)
}

// Search returns the first limit IDs of the matching ordering, scored
// descending so the caller sees a plausible ranked shape. (The engine
// keeps the rank, not the score, so the numbers are cosmetic.)
func (o *orderedBackend) Search(query string, limit int) []search.SearchResult {
	o.mu.Lock()
	o.limits = append(o.limits, limit)
	o.mu.Unlock()

	ids := o.ordering(query)
	if limit > 0 && len(ids) > limit {
		ids = ids[:limit]
	}
	out := make([]search.SearchResult, 0, len(ids))
	for i, id := range ids {
		out = append(out, search.SearchResult{ID: id, Score: float64(len(ids) - i)})
	}
	return out
}

func (o *orderedBackend) ordering(query string) []string {
	var (
		seen = make(map[string]bool)
		ids  []string
	)
	for _, tok := range orderedQueryTokens(query) {
		for _, id := range o.byToken[tok] {
			if seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return o.def
	}
	return ids
}

// orderedQueryTokens lowercases the query, splits it on whitespace and
// trims edge punctuation from each field.
func orderedQueryTokens(query string) []string {
	fields := strings.Fields(strings.ToLower(query))
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimFunc(f, func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}
