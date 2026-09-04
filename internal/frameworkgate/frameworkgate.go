// Package frameworkgate resolves the configured framework allow-list into
// a matcher the framework registries can consult.
//
// Gortex ships framework intelligence in three independent registries —
// the extract-time route passes (internal/contracts), the post-resolution
// dispatch synthesizers and the claiming resolvers (internal/resolver).
// A repository that uses none of a given framework still pays for its
// passes on every index, and the heuristic edges an unused framework
// synthesizes are pure graph noise, so `index.frameworks.allow` narrows
// the active set across all three at once.
//
// This package exists rather than a helper inside internal/config because
// internal/resolver and internal/contracts deliberately do not import
// internal/config (cycle avoidance — see resolver/cross_repo.go and
// contracts/event_bus.go). A leaf package is the only shape all three
// layers plus config can share, and sharing matters: the wildcard and
// union semantics must not drift between the layer that gates extraction
// and the layer that gates synthesis.
package frameworkgate

import (
	"sort"
	"strings"
)

// Set is an immutable, resolved framework allow-list.
//
// An UNSET Set allows everything. This is the load-bearing default: an
// absent `index.frameworks.allow` key, a repository with no .gortex.yaml,
// and the zero value must all mean "every framework runs", never "no
// framework runs". Callers can therefore pass a Set by value with no nil
// handling, and forgetting to configure one cannot silently blind the
// graph.
type Set struct {
	exact  map[string]bool
	prefix []string
	all    bool
	// configured distinguishes "an allow-list was given" from the unset
	// Set that allows everything.
	configured bool
	raw        []string
}

// New resolves configured patterns into an allow-list.
//
// Matching is deliberately narrow: an entry is either a bare "*" (allow
// everything explicitly), a trailing-"*" prefix match, or an exact name.
// path.Match is NOT used, because framework names contain "-", "." and
// "/" (celery-dispatch, react-native-bridge, net/http) and a user must
// never have to think about whether those are metacharacters.
//
// Entries are trimmed and lowercased; all three registries mint
// lower-kebab names, so case-insensitive matching is safe and forgiving.
//
// A slice that is nil, empty, or contains only blanks yields the unset
// Set — which allows everything. Narrowing to nothing is spelled with an
// explicit sentinel, not by writing an empty list; see AllowNone.
// Repeated entries collapse to the first spelling seen, compared
// case-insensitively for the same reason matching is. Union is defined as
// New(a.Patterns() + b.Patterns()), so folding N repositories that allow the
// same framework used to yield that name N times in raw. Nothing about
// ADMISSION noticed — exact is a map, prefix matches by OR, and all is a bool,
// so Allows is a function of the underlying SET — but Patterns() is the union's
// only observable, and callers that fingerprint it saw a value that moved with
// the number of repositories rather than with the allow-list. That cost a real
// bug: tracking a sixth repository whose list was identical to the other five
// changed indexer.DeriveConfigHash, and every previously-derived repository
// then read "partial: derive-relevant config changed" and advertised a
// whole-workspace re-derive that would have emitted identical edges.
func New(patterns []string) Set {
	s := Set{}
	seen := map[string]bool{}
	for _, p := range patterns {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" {
			continue
		}
		// Set before the dedupe check: a list of nothing but repeats is still
		// a configured list, and reading it as unset would re-admit the whole
		// registry — the one direction this package must never drift in.
		s.configured = true
		lowered := strings.ToLower(trimmed)
		if seen[lowered] {
			continue
		}
		seen[lowered] = true
		s.raw = append(s.raw, trimmed)
		switch {
		case lowered == "*":
			s.all = true
		case lowered == allowNoneSentinel:
			// Explicit "allow nothing": recorded, but adds no matcher
			// entry, so nothing is admitted.
		case strings.HasSuffix(lowered, "*"):
			stem := strings.TrimSuffix(lowered, "*")
			if stem == "" {
				s.all = true
				continue
			}
			s.prefix = append(s.prefix, stem)
		default:
			if s.exact == nil {
				s.exact = map[string]bool{}
			}
			s.exact[lowered] = true
		}
	}
	sort.Strings(s.prefix)
	return s
}

// allowNoneSentinel spells "run no framework at all". An empty list
// cannot mean this: an empty list is indistinguishable from an absent
// key, and resolving that to "nothing runs" would make a stray blank
// block silently strip every framework edge from the graph.
const allowNoneSentinel = "none"

// AllowNone returns the Set that admits no framework.
func AllowNone() Set { return New([]string{allowNoneSentinel}) }

// Allows reports whether the named pass may run.
//
// An unset Set allows every name — see the type doc.
func (s Set) Allows(name string) bool {
	if !s.configured || s.all {
		return true
	}
	lowered := strings.ToLower(strings.TrimSpace(name))
	if lowered == "" {
		return false
	}
	if s.exact[lowered] {
		return true
	}
	for _, p := range s.prefix {
		if strings.HasPrefix(lowered, p) {
			return true
		}
	}
	return false
}

// Configured reports whether an allow-list was given at all. An unset Set
// allows everything, so hot paths can skip the matcher entirely.
func (s Set) Configured() bool { return s.configured }

// AllowsNothing reports whether this Set admits no name at all — the
// AllowNone sentinel, or a configured list that resolved to no matcher.
//
// It exists so a caller can skip an ENTIRE pipeline rather than calling
// Allows once per pass and doing the surrounding setup regardless. Note the
// asymmetry with Allows: an unset Set allows everything, so it is never
// "nothing", and the zero value correctly reports false.
func (s Set) AllowsNothing() bool {
	return s.configured && !s.all && len(s.exact) == 0 && len(s.prefix) == 0
}

// Patterns returns the configured entries as written, for diagnostics.
func (s Set) Patterns() []string {
	out := make([]string, len(s.raw))
	copy(out, s.raw)
	return out
}

// Unknown returns the exact-form entries that name no registered pass —
// the typo report ("celery_dispatch" instead of "celery-dispatch"). On an
// allow-list a typo is worse than on a deny-list: it does not merely fail
// to take effect, it silently drops the framework the user meant to keep.
//
// Wildcard entries are exempt: a prefix may legitimately match nothing
// today and match a pass added in a later release, and warning about it
// would train users to ignore the warning.
func (s Set) Unknown(known []string) []string {
	if len(s.exact) == 0 {
		return nil
	}
	have := make(map[string]bool, len(known))
	for _, k := range known {
		have[strings.ToLower(strings.TrimSpace(k))] = true
	}
	var out []string
	for name := range s.exact {
		if !have[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// Union returns the Set allowing everything either input allows.
//
// This is the multi-repository rule. The global synthesis pass writes
// into one shared graph, so a framework narrowed away on one repository's
// say-so would also strip a sibling repository's edges, and that sibling
// never opted out. Admission is therefore permissive: a repository that
// did not restrict a framework keeps it. It is the same shape as the
// shipped MultiIndexer.externalCallSynthesisEnabled OR-union, so both
// workspace-wide toggles follow one rule.
//
// Because an unset Set allows everything, a union with one is itself
// unset — a single unconfigured repository re-admits the full registry,
// which is the safe direction.
func Union(a, b Set) Set {
	if !a.configured {
		return a
	}
	if !b.configured {
		return b
	}
	return New(append(a.Patterns(), b.Patterns()...))
}

// Intersect returns the Set allowing only what BOTH inputs allow. It is
// the strict counterpart to Union, for callers that own a single graph
// scope and want the narrowest admission rather than the safest.
//
// The intersection is taken over what the Sets MATCH, not over how they
// are spelled. Comparing raw patterns made Intersect disagree with
// Allows on every wildcard: `*` ∩ `django` is `django`, not nothing, and
// `godot*` ∩ `godot-autoload` is `godot-autoload`, but a string
// comparison finds no shared entry in either pair and narrows to
// AllowNone — the silent-blinding direction this package exists to keep
// out of the graph.
func Intersect(a, b Set) Set {
	if !a.configured || a.all {
		return b
	}
	if !b.configured || b.all {
		return a
	}
	var keep []string
	seen := map[string]bool{}
	add := func(p string) {
		lowered := strings.ToLower(p)
		if seen[lowered] {
			return
		}
		seen[lowered] = true
		keep = append(keep, p)
	}
	// An exact name survives if the other side admits it, by whichever
	// rule — an equal name or a prefix that covers it.
	for _, p := range a.raw {
		if !isWildcardPattern(p) && b.Allows(p) {
			add(p)
		}
	}
	for _, p := range b.raw {
		if !isWildcardPattern(p) && a.Allows(p) {
			add(p)
		}
	}
	// Two prefixes overlap only when one stem extends the other, and the
	// longer stem is then exactly their intersection. Disjoint stems
	// (`godot*` ∩ `django*`) share nothing and contribute nothing.
	for _, pa := range a.prefix {
		for _, pb := range b.prefix {
			switch {
			case strings.HasPrefix(pa, pb):
				add(pa + "*")
			case strings.HasPrefix(pb, pa):
				add(pb + "*")
			}
		}
	}
	if len(keep) == 0 {
		return AllowNone()
	}
	return New(keep)
}

// isWildcardPattern reports whether an entry is written as a prefix match
// rather than an exact name. Prefix stems intersect by containment, not by
// membership, so the two forms are combined separately.
func isWildcardPattern(p string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(p)), "*")
}
