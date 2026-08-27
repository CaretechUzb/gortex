package graph

import "sync"

// Checkout groups: which tracked repositories are separate checkouts of
// one underlying repository.
//
// Gortex tracks a repository by its root directory, so a `git worktree`
// of an already-tracked repo is registered as an independent repo with
// its own prefix. Nothing in the graph then distinguishes "two repos
// that happen to share code" from "one repo checked out twice", and
// every name-keyed resolution pass — the Odoo model binder, JS import
// resolution, implements/overrides inference, the cross_repo_* edge
// layer — sees a second, byte-identical definition of every symbol and
// binds to it. Measured on one 117k-node repository tracked alongside a
// worktree of itself: ~190k edges crossing the two checkouts, including
// classes that "extend" themselves in the other checkout.
//
// Those edges are structurally valid and semantically meaningless. A
// checkout group is the missing fact that lets a pass reject them: two
// prefixes in the same group are the same code, so an edge between them
// never carries information.
//
// The grouping is daemon-scoped runtime topology, not graph content: the
// indexer republishes it whenever a repository is tracked, untracked, or
// reconciled, and it is deliberately NOT persisted — a store reopened by
// a daemon that no longer tracks the worktree must not keep believing in
// it.
//
// The zero value groups nothing, which is the load-bearing default: an
// unwired store, a test graph, and a single-repo workspace must all mean
// "no two prefixes are siblings", never "everything is a sibling".

// CheckoutGroups is the mutable, non-persisted repo-prefix → checkout-group
// map. Embed it in a Store implementation to give every pass the sibling
// test through the CheckoutGrouped interface below.
type CheckoutGroups struct {
	mu       sync.RWMutex
	byPrefix map[string]string
}

// SetCheckoutGroups replaces the whole grouping. Callers pass repo prefix
// → checkout identity (in practice the shared .git directory every
// worktree of a repository resolves to); a prefix whose identity is
// unknown is simply omitted and is then a sibling of nothing.
//
// Replacement rather than merge is deliberate: the indexer publishes the
// complete current topology, so an untracked repo has to disappear from
// the map rather than linger as a phantom sibling.
func (c *CheckoutGroups) SetCheckoutGroups(byPrefix map[string]string) {
	next := make(map[string]string, len(byPrefix))
	for prefix, group := range byPrefix {
		if prefix == "" || group == "" {
			continue
		}
		next[prefix] = group
	}
	c.mu.Lock()
	if len(next) == 0 {
		c.byPrefix = nil
	} else {
		c.byPrefix = next
	}
	c.mu.Unlock()
}

// CheckoutGroup returns the checkout identity of a repo prefix, or "" when
// the prefix is unknown or ungrouped.
func (c *CheckoutGroups) CheckoutGroup(repoPrefix string) string {
	if c == nil || repoPrefix == "" {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.byPrefix[repoPrefix]
}

// HasCheckoutGroups reports whether any grouping is published at all. It
// is the cheap short-circuit every hot-path caller takes first: with no
// worktrees tracked — the overwhelmingly common case — the sibling test
// costs one atomic-free map-length read and no prefix parsing.
func (c *CheckoutGroups) HasCheckoutGroups() bool {
	if c == nil {
		return false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byPrefix) > 0
}

// CheckoutGrouped is the optional Store capability the sibling test needs.
// A store that does not implement it groups nothing.
type CheckoutGrouped interface {
	CheckoutGroup(repoPrefix string) string
	HasCheckoutGroups() bool
}

// SiblingCheckouts reports whether two repo prefixes are separate checkouts
// of the same repository.
//
// It is false for a prefix compared with itself: that is the SAME checkout,
// not a sibling, and callers use this to reject cross-checkout work that
// same-repo work must still be allowed to do. It is false for an empty
// prefix on either side, so the repo-independent namespaces (dep::,
// external::, unresolved::, module::) are never suppressed. And it is
// false whenever the store publishes no grouping, so an unwired caller
// keeps its previous behaviour exactly.
func SiblingCheckouts(store any, a, b string) bool {
	if a == "" || b == "" || a == b {
		return false
	}
	grouped, ok := store.(CheckoutGrouped)
	if !ok || !grouped.HasCheckoutGroups() {
		return false
	}
	ga := grouped.CheckoutGroup(a)
	return ga != "" && ga == grouped.CheckoutGroup(b)
}

// SiblingCheckoutIDs is SiblingCheckouts for two node IDs whose repo
// prefixes have to be recovered syntactically. It short-circuits on the
// store BEFORE parsing either ID, so the common ungrouped workspace pays
// nothing per edge.
func SiblingCheckoutIDs(store any, fromID, toID string) bool {
	grouped, ok := store.(CheckoutGrouped)
	if !ok || !grouped.HasCheckoutGroups() {
		return false
	}
	return SiblingCheckouts(store, RepoPrefixOfID(fromID), RepoPrefixOfID(toID))
}

// DistinctCheckoutGroups counts how many distinct repositories a set of
// prefixes actually represents. A prefix with no published group counts
// as its own repository — the correct reading of "unknown", and the one
// that keeps an unwired workspace's count equal to its prefix count.
//
// This is what a pass should compare against 2 when deciding whether
// cross-repository work is possible at all: a workspace holding a repo
// and three worktrees of it has four prefixes and one repository, and no
// edge between any two of them can be a genuine cross-repo relationship.
func DistinctCheckoutGroups(store any, prefixes []string) int {
	grouped, _ := store.(CheckoutGrouped)
	seen := make(map[string]struct{}, len(prefixes))
	for _, prefix := range prefixes {
		if prefix == "" {
			continue
		}
		key := prefix
		if grouped != nil {
			if group := grouped.CheckoutGroup(prefix); group != "" {
				key = "\x00group\x00" + group
			}
		}
		seen[key] = struct{}{}
	}
	return len(seen)
}

// SetCheckoutGroups publishes the repo-prefix → checkout-group map for
// this in-memory graph. See CheckoutGroups.SetCheckoutGroups.
func (g *Graph) SetCheckoutGroups(byPrefix map[string]string) {
	g.checkoutGroups.SetCheckoutGroups(byPrefix)
}

// CheckoutGroup implements CheckoutGrouped.
func (g *Graph) CheckoutGroup(repoPrefix string) string {
	return g.checkoutGroups.CheckoutGroup(repoPrefix)
}

// HasCheckoutGroups implements CheckoutGrouped.
func (g *Graph) HasCheckoutGroups() bool { return g.checkoutGroups.HasCheckoutGroups() }

// HasSiblingCheckouts reports whether a store publishes any checkout
// grouping at all. Callers use it to skip allocation-bearing filtering
// entirely on the workspaces — the overwhelming majority — where no two
// prefixes can be siblings.
func HasSiblingCheckouts(store any) bool {
	grouped, ok := store.(CheckoutGrouped)
	return ok && grouped.HasCheckoutGroups()
}

var _ CheckoutGrouped = (*Graph)(nil)
