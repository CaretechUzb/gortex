package search

import (
	"sort"
	"strings"
)

// EquivalenceTable is a deterministic, LLM-free synonym table over
// universal software-concept classes. Each class is a set of words
// that name the same idea across virtually every codebase ("auth" /
// "authentication" / "login" / "signin"; "delete" / "remove" /
// "destroy"). Expand returns a token's class siblings so the search
// layer can bridge query vocabulary to the words a symbol actually
// uses -- the deterministic complement to LLM query expansion, and
// the only expansion available when no LLM provider is configured.
//
// The table is curated and intentionally conservative: only classes
// whose members are genuinely interchangeable in code identifiers
// belong here. Domain-bearing words that mean different things in
// different codebases are left out -- a false synonym inflates the
// text candidate pool with noise.
type EquivalenceTable struct {
	// member maps each lowercased word to the index of its class in
	// classes. A word in two classes keeps the first; the curated
	// table is built to avoid overlap.
	member  map[string]int
	classes [][]string

	// related is the concept-relatedness thesaurus: it maps a class
	// index to the OTHER classes that are conceptually adjacent but
	// NOT interchangeable -- "auth" pulls in "token" / "session" /
	// "jwt", which name different things yet co-occur in the same
	// problem space. This is deliberately SEPARATE from the union-find
	// classes: folding these into the synonym classes would merge
	// distinct concepts into one giant class and destroy precision.
	// ExpandRelated walks it at a lower priority than the direct
	// synonym siblings Expand returns.
	related map[int][]relatedClass
}

// relatedClass is one edge in the concept-relatedness thesaurus: the
// index of a conceptually-adjacent class plus a weight in (0, 1]. The
// weight is advisory -- the search layer uses it only to order /
// bound the related terms, never to merge classes.
type relatedClass struct {
	idx    int
	weight float64
}

// curatedClasses is the compiled-in baseline. Style mirrors the
// static expansionStoplist / assistStopWords tables -- a flat literal
// kept short and reviewed by hand. Each inner slice is one class.
var curatedClasses = [][]string{
	{"auth", "authentication", "authenticate", "login", "signin", "logon", "credential", "credentials"},
	{"authz", "authorization", "authorize", "permission", "permissions", "acl", "rbac"},
	{"delete", "remove", "destroy", "drop", "erase", "purge", "unlink"},
	{"create", "add", "new", "make", "insert", "register"},
	{"update", "modify", "edit", "change", "patch", "mutate"},
	{"fetch", "get", "retrieve", "load", "read", "lookup"},
	{"save", "store", "persist", "write", "commit", "flush"},
	{"config", "configuration", "configure", "settings", "options", "preferences"},
	{"error", "err", "fault", "failure", "exception"},
	{"validate", "validation", "verify", "check", "assert", "ensure"},
	{"parse", "parser", "decode", "deserialize", "unmarshal", "unpack"},
	{"encode", "serialize", "marshal", "pack", "format"},
	{"connect", "connection", "dial", "session", "socket"},
	{"close", "disconnect", "shutdown", "teardown", "dispose"},
	{"start", "begin", "init", "initialize", "bootstrap", "launch", "boot"},
	{"stop", "halt", "cancel", "abort", "terminate", "kill"},
	{"send", "publish", "emit", "dispatch", "post", "push"},
	{"receive", "consume", "subscribe", "listen", "handle", "recv"},
	{"encrypt", "encryption", "cipher", "crypt"},
	{"decrypt", "decryption", "decipher"},
	{"cache", "caching", "memoize", "memoise"},
	{"queue", "buffer", "backlog", "pipeline"},
	{"log", "logger", "logging", "trace", "tracer"},
	{"metric", "metrics", "telemetry", "instrumentation", "stats"},
	{"retry", "retries", "backoff", "reattempt"},
	{"throttle", "ratelimit", "ratelimiter", "debounce"},
	{"user", "account", "member", "profile"},
	{"request", "req", "query"},
	{"response", "resp", "reply", "result"},
	{"token", "jwt", "bearer", "apikey"},
	{"hash", "digest", "checksum", "fingerprint"},
	{"middleware", "interceptor", "filter", "hook"},
	{"migrate", "migration", "schema"},
	{"index", "indexer", "indexing"},
	{"search", "query", "find", "lookup"},
}

// conceptRelations is the curated concept-relatedness thesaurus. Each
// entry pairs a representative word of one class with representative
// words of OTHER classes that are conceptually adjacent -- they name
// different things but live in the same problem space, so a query for
// one often wants symbols built from the others. These are NOT
// synonyms: they must never be folded into the union-find classes
// (that would merge distinct concepts). The relation is made
// symmetric at build time. A weight tunes how strongly a related
// class is pulled in; all curated relations sit below 1.0 so a
// related term always ranks behind a direct synonym.
//
// Representative words are looked up against the curated classes at
// build time; an entry naming a word that is in no class is skipped.
var conceptRelations = []struct {
	from    string
	related []string
}{
	{"auth", []string{"token", "session", "user"}},
	{"authz", []string{"auth", "user", "token"}},
	{"token", []string{"auth", "session", "encrypt"}},
	{"session", []string{"auth", "connect", "token"}},
	{"encrypt", []string{"decrypt", "hash", "token"}},
	{"decrypt", []string{"encrypt", "hash"}},
	{"hash", []string{"encrypt", "token"}},
	{"cache", []string{"save", "fetch", "queue"}},
	{"queue", []string{"send", "receive", "cache"}},
	{"send", []string{"receive", "queue"}},
	{"receive", []string{"send", "queue"}},
	{"retry", []string{"throttle", "error"}},
	{"throttle", []string{"retry", "queue"}},
	{"log", []string{"metric", "error"}},
	{"metric", []string{"log"}},
	{"error", []string{"log", "retry", "validate"}},
	{"validate", []string{"error", "parse"}},
	{"parse", []string{"encode", "validate"}},
	{"encode", []string{"parse"}},
	{"migrate", []string{"index", "save"}},
	{"middleware", []string{"auth", "request", "response"}},
	{"request", []string{"response", "middleware", "user"}},
	{"response", []string{"request", "middleware"}},
}

// Concept-relation pull weights. Both sit below 1.0 so a thesaurus
// term always ranks behind a direct synonym in any weight-ordered
// consumer. The forward weight (a class's OWN declared relations) is
// higher than the reverse weight (edges inferred by symmetry) so a
// query for "auth" pulls its directly-declared neighbours (token /
// session / user) ahead of classes that merely declared a relation
// back to auth.
const (
	relatedWeightForward = 0.6
	relatedWeightReverse = 0.45
	// relatedPositionDecay reduces a forward edge's weight by its
	// position in the class's declared related list, so the
	// first-declared neighbour ranks highest. Small enough that the
	// first ~3 declared neighbours all stay above the reverse weight.
	relatedPositionDecay = 0.03
)

// NewEquivalenceTable builds the curated table plus any repo-supplied
// extra classes. extra maps a class label to its member words; the
// label itself joins the class so a search for the label hits every
// member. Words in extra are merged into an existing class when they
// already belong to one, so a project can extend a curated class
// rather than fork it.
func NewEquivalenceTable(extra map[string][]string) *EquivalenceTable {
	t := &EquivalenceTable{member: map[string]int{}}
	for _, class := range curatedClasses {
		t.addClass(class)
	}
	for label, words := range extra {
		members := make([]string, 0, len(words)+1)
		if l := strings.ToLower(strings.TrimSpace(label)); l != "" {
			members = append(members, l)
		}
		for _, w := range words {
			if l := strings.ToLower(strings.TrimSpace(w)); l != "" {
				members = append(members, l)
			}
		}
		t.addClass(members)
	}
	t.buildRelated()
	return t
}

// buildRelated compiles the curated conceptRelations into class-index
// edges. Each relation word is resolved to its class; a word in no
// class is skipped. Edges are symmetric (a->b implies b->a) and
// deduplicated per source class. Self-edges (both words in the same
// class) are dropped -- those are synonyms Expand already covers.
func (t *EquivalenceTable) buildRelated() {
	t.related = map[int][]relatedClass{}
	// best tracks the strongest weight seen per (a,b) edge so a forward
	// declaration always wins over a reverse-inferred one regardless of
	// the order conceptRelations is walked in.
	best := map[[2]int]float64{}
	link := func(a, b int, weight float64) {
		if a == b {
			return
		}
		key := [2]int{a, b}
		if w, ok := best[key]; ok {
			if weight <= w {
				return
			}
			// Upgrade the existing edge's weight in place.
			for i := range t.related[a] {
				if t.related[a][i].idx == b {
					t.related[a][i].weight = weight
					break
				}
			}
			best[key] = weight
			return
		}
		best[key] = weight
		t.related[a] = append(t.related[a], relatedClass{idx: b, weight: weight})
	}
	for _, rel := range conceptRelations {
		fromIdx, ok := t.member[strings.ToLower(strings.TrimSpace(rel.from))]
		if !ok {
			continue
		}
		for pos, w := range rel.related {
			toIdx, ok := t.member[strings.ToLower(strings.TrimSpace(w))]
			if !ok {
				continue
			}
			// Decay the forward weight by declaration order so a
			// class's FIRST-declared neighbour (the most relevant, by
			// the curator's intent: auth -> token before auth ->
			// session) ranks ahead of later ones. The decay is small
			// enough to stay above the reverse weight for the first
			// few positions, so genuinely-declared neighbours still
			// outrank symmetry-inferred edges.
			fwd := relatedWeightForward - float64(pos)*relatedPositionDecay
			if fwd < relatedWeightReverse {
				fwd = relatedWeightReverse
			}
			link(fromIdx, toIdx, fwd)
			link(toIdx, fromIdx, relatedWeightReverse) // symmetric, weaker
		}
	}
}

// addClass folds one class into the table. If any word already maps
// to a class, every other word in the new group is merged into that
// existing class; otherwise a fresh class is appended. Empty and
// duplicate words are dropped.
func (t *EquivalenceTable) addClass(words []string) {
	clean := make([]string, 0, len(words))
	seen := map[string]struct{}{}
	for _, w := range words {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		if _, dup := seen[w]; dup {
			continue
		}
		seen[w] = struct{}{}
		clean = append(clean, w)
	}
	if len(clean) < 2 {
		return
	}
	// Find an existing class any word already belongs to.
	target := -1
	for _, w := range clean {
		if idx, ok := t.member[w]; ok {
			target = idx
			break
		}
	}
	if target < 0 {
		target = len(t.classes)
		t.classes = append(t.classes, nil)
	}
	for _, w := range clean {
		if _, ok := t.member[w]; ok {
			continue
		}
		t.member[w] = target
		t.classes[target] = append(t.classes[target], w)
	}
}

// Expand returns the class siblings of token -- every other word in
// its equivalence class -- or nil when the token is in no class. The
// token itself is never included. Lookup is case-insensitive.
func (t *EquivalenceTable) Expand(token string) []string {
	if t == nil {
		return nil
	}
	tok := strings.ToLower(strings.TrimSpace(token))
	if tok == "" {
		return nil
	}
	idx, ok := t.member[tok]
	if !ok {
		return nil
	}
	class := t.classes[idx]
	out := make([]string, 0, len(class)-1)
	for _, w := range class {
		if w != tok {
			out = append(out, w)
		}
	}
	return out
}

// ExpandRelated returns the concept-relatedness siblings of token --
// the members of every class conceptually adjacent to token's class
// in the curated thesaurus, in descending relation weight (ties
// broken by word, deterministically). These are NOT synonyms: "auth"
// relates to "token" / "session" / "jwt", which name distinct ideas
// that share a problem space. The caller is expected to feed them to
// retrieval at a LOWER priority than Expand's direct siblings, and to
// keep their ranking effect modest, so a related-concept bridge
// widens recall without eroding precision.
//
// The token's own class members and the query token itself are never
// returned -- those are Expand's job. Returns nil when the token is in
// no class or its class has no curated relations. Lookup is
// case-insensitive.
func (t *EquivalenceTable) ExpandRelated(token string) []string {
	if t == nil {
		return nil
	}
	tok := strings.ToLower(strings.TrimSpace(token))
	if tok == "" {
		return nil
	}
	idx, ok := t.member[tok]
	if !ok {
		return nil
	}
	edges := t.related[idx]
	if len(edges) == 0 {
		return nil
	}
	// Stable order: by descending weight, then by class index, so the
	// emitted term order is deterministic across runs.
	ordered := make([]relatedClass, len(edges))
	copy(ordered, edges)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].weight != ordered[j].weight {
			return ordered[i].weight > ordered[j].weight
		}
		return ordered[i].idx < ordered[j].idx
	})

	var (
		out  []string
		seen = map[string]struct{}{}
	)
	for _, e := range ordered {
		if e.idx < 0 || e.idx >= len(t.classes) {
			continue
		}
		for _, w := range t.classes[e.idx] {
			if w == tok {
				continue
			}
			if _, dup := seen[w]; dup {
				continue
			}
			seen[w] = struct{}{}
			out = append(out, w)
		}
	}
	return out
}

// ClassCount reports the number of equivalence classes -- curated
// plus any merged-in repo extras. Used by tests and diagnostics.
func (t *EquivalenceTable) ClassCount() int {
	if t == nil {
		return 0
	}
	return len(t.classes)
}

// RelatedClassCount reports the number of classes that have at least
// one curated concept-relation edge. Used by tests and diagnostics.
func (t *EquivalenceTable) RelatedClassCount() int {
	if t == nil {
		return 0
	}
	return len(t.related)
}
