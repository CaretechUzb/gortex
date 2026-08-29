package resolver

import "github.com/zzet/gortex/internal/graph"

// Repository-aware Odoo lookup indexes.
//
// Every Odoo binder joins a name to a node through a graph-wide index:
// an external ID to the `<record>` declaring it, a model `_name` to the
// Python classes implementing it, a QWeb template to its markup, a module
// specifier to its file. Those indexes used to keep one target per key —
// the lexicographically lowest node ID — which is deterministic but
// repository-blind, and lexical order has nothing to do with which
// repository asked.
//
// Two things go wrong once a workspace holds more than one repository.
// A reference in repo A binds to repo B's identically-named record
// whenever B's node ID sorts first, even though A declares its own. And
// when B is a git worktree of A (see graph/checkout_groups.go) EVERY key
// has a duplicate, so essentially every Odoo edge crosses into the wrong
// checkout: measured on one Odoo workspace tracked alongside a worktree
// of itself, ~190k edges bound to the sibling checkout, including classes
// that "extend" themselves.
//
// Keeping candidates per repository fixes both. The asking edge's own
// repository wins outright; a sibling checkout of it never wins at all;
// everything else falls back to the previous lowest-ID rule, so genuine
// cross-repository Odoo layouts — a custom addon inheriting a model that
// only core declares — resolve exactly as before.

// odooIndex maps a lookup key to the best candidate node per repository
// prefix. Nodes with no repo prefix (single-repo graphs) group under "".
type odooIndex map[string]map[string]string

// put records a candidate, keeping the lowest node ID per repository so
// the index stays deterministic across runs within each repository.
func (ix odooIndex) put(key, id string) {
	if key == "" || id == "" {
		return
	}
	repo := graph.RepoPrefixOfID(id)
	byRepo := ix[key]
	if byRepo == nil {
		byRepo = map[string]string{}
		ix[key] = byRepo
	}
	if prev, ok := byRepo[repo]; !ok || id < prev {
		byRepo[repo] = id
	}
}

// lookup resolves key for an edge whose source node is fromID.
//
// Order: the asking repository's own candidate, then the lowest-ID
// candidate from any repository that is not a sibling checkout of it.
// Empty when the key is unknown, or when every candidate is a duplicate
// living in a sibling checkout.
//
// The sibling test comes from the pass's odooSiblingCache rather than
// straight from the store: this runs once per candidate repository per
// lookup, and every lookup in one pass asks about the same handful of
// prefixes.
func (ix odooIndex) lookup(sc *odooSiblingCache, fromID, key string) string {
	byRepo := ix[key]
	if len(byRepo) == 0 {
		return ""
	}
	askingRepo := graph.RepoPrefixOfID(fromID)
	if id := byRepo[askingRepo]; id != "" {
		return id
	}
	best := ""
	for repo, id := range byRepo {
		if sc.siblings(askingRepo, repo) {
			continue
		}
		if best == "" || id < best {
			best = id
		}
	}
	return best
}

// odooSiblingCache memoizes the sibling-checkout test, and the fan-out
// filtering built on it, for the duration of ONE Odoo pass.
//
// The test itself is cheap — a length check and two map reads behind an
// RWMutex — but the binders ask it once per candidate target, per edge,
// and odoo.go notes that the Odoo families run to a million edges on a
// full workspace. It is only ever non-trivial when a worktree is tracked
// beside its repository, and that is exactly when it is asked most: on a
// workspace holding three checkouts of one repository, one measured pass
// built and then discarded 694,422 cross-checkout candidates, taking two
// RLocks and parsing two node IDs for each.
//
// Both layers are memoizable outright because neither input moves during
// a pass. Repo prefixes are a handful of fixed strings, and every index a
// binder filters against is built once at the head of that binder and not
// mutated afterwards — so the filtered target list for one (asking
// repository, lookup key) pair is the same answer every time it is asked.
// That turns the per-edge cost from O(candidates) into a map read, and
// bounds the total from O(edges x candidates) to O(repos x keys x
// candidates).
//
// NOT safe for concurrent use, and deliberately so: the synthesizer loop
// is serial by construction (the registration order in
// defaultFrameworkSynthesizers is load-bearing, and every write funnels
// through one edge batch), so a mutex here would cost something and buy
// nothing. A cache is scoped to one ResolveOdooRefsScoped call and dies
// with it.
type odooSiblingCache struct {
	store  any
	active bool
	pair   map[[2]string]bool
	kept   map[[2]string][]string
	member map[[2]string]map[string]bool
}

// newOdooSiblingCache resolves the one question that decides whether any
// of this costs anything: whether the store publishes a checkout grouping
// at all. On the overwhelmingly common workspace that tracks no worktree,
// active is false and every method below short-circuits to the identity.
func newOdooSiblingCache(store any) *odooSiblingCache {
	return &odooSiblingCache{store: store, active: graph.HasSiblingCheckouts(store)}
}

// siblings is graph.SiblingCheckouts memoized on the prefix pair.
func (c *odooSiblingCache) siblings(a, b string) bool {
	if c == nil || !c.active || a == "" || b == "" || a == b {
		return false
	}
	key := [2]string{a, b}
	if v, ok := c.pair[key]; ok {
		return v
	}
	v := graph.SiblingCheckouts(c.store, a, b)
	if c.pair == nil {
		c.pair = map[[2]string]bool{}
	}
	c.pair[key] = v
	return v
}

// keep filters a fan-out target list down to the targets that are not
// duplicates living in a sibling checkout of the asking node's repository.
//
// The model binder fans out on purpose — several addons legitimately
// extend one `_name` — so its candidates cannot be collapsed to one the
// way odooIndex.lookup collapses a unique key. What they can be is
// deduplicated across checkouts: a second checkout contributes no new
// declaring class, only the same class again under a foreign prefix.
//
// key identifies the target list, so callers MUST pass the same key they
// looked the list up by; passing an unrelated key would serve one list's
// verdict for another.
func (c *odooSiblingCache) keep(fromID, key string, targets []string) []string {
	if c == nil || !c.active || len(targets) == 0 {
		return targets
	}
	asking := graph.RepoPrefixOfID(fromID)
	ck := [2]string{asking, key}
	if v, ok := c.kept[ck]; ok {
		return v
	}
	kept := make([]string, 0, len(targets))
	for _, target := range targets {
		if c.siblings(asking, graph.RepoPrefixOfID(target)) {
			continue
		}
		kept = append(kept, target)
	}
	if c.kept == nil {
		c.kept = map[[2]string][]string{}
	}
	c.kept[ck] = kept
	return kept
}

// declares reports whether target is still one of the targets key admits
// for fromID's repository — the set-membership form of keep.
//
// This is the predicate fan-out retirement runs, and it used to be a
// linear scan of the whole candidate list per observed sibling edge. On a
// re-derive the graph already holds the previous pass's siblings, so that
// was O(existing siblings x declarations per model) string comparisons
// over the widest models in the corpus.
//
// It asks the FILTERED list on purpose. Retirement has to run the same
// predicate as binding, or the two disagree: a sibling edge reaching into
// a sibling checkout is one the binder would refuse to create today, so
// leaving it un-retired would preserve exactly the cross-checkout edges
// the checkout group exists to keep out (graph/checkout_groups.go).
func (c *odooSiblingCache) declares(fromID, key string, targets []string, target string) bool {
	kept := c.keep(fromID, key, targets)
	if len(kept) == 0 {
		return false
	}
	ck := [2]string{graph.RepoPrefixOfID(fromID), key}
	set, ok := c.member[ck]
	if !ok {
		set = make(map[string]bool, len(kept))
		for _, t := range kept {
			set[t] = true
		}
		if c.member == nil {
			c.member = map[[2]string]map[string]bool{}
		}
		c.member[ck] = set
	}
	return set[target]
}
