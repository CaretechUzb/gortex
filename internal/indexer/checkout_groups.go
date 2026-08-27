package indexer

// Publication of repository checkout groups.
//
// Tracking a git worktree of an already-tracked repository gives the
// workspace two prefixes over one body of code. Nothing downstream can
// tell that apart from two genuinely independent repos, so every
// name-keyed pass binds across the pair and manufactures edges that are
// structurally valid and semantically empty (see
// graph/checkout_groups.go for the measured damage).
//
// The indexer is the only layer that knows a repo's root path, so it is
// the only layer that can compute the grouping. It publishes the map
// into the graph store, where every pass can consult it without a new
// parameter on a dozen call chains.

// publishCheckoutGroups recomputes the repo-prefix → checkout-group map
// from the tracked repository roots and publishes it to the store.
//
// Only prefixes that actually SHARE a checkout with another prefix are
// published. A workspace with no worktrees therefore publishes an empty
// map, which is what lets every consumer short-circuit on
// HasCheckoutGroups() before doing any per-edge work.
//
// Call it after any change to the tracked repository set. It is cheap —
// two stats and a small file read per repo — and idempotent, so an extra
// call is only ever wasted work, never a wrong grouping.
func (mi *MultiIndexer) publishCheckoutGroups() {
	sink, ok := mi.graph.(interface {
		SetCheckoutGroups(map[string]string)
	})
	if !ok {
		return
	}
	sink.SetCheckoutGroups(mi.checkoutGroups())
}

// checkoutGroups returns repo prefix → checkout-group identity for every
// tracked prefix that shares its checkout with at least one other.
//
// The identity is the repo's main-worktree root with symlinks evaluated —
// the same key LinkedWorktreeRoots compares on, so "these two prefixes
// are worktrees of one repository" means exactly the same thing in both
// places. A path that is not a git repository resolves to itself and so
// groups with nothing, which is the correct answer for two unrelated
// directories that happen to hold identical files.
func (mi *MultiIndexer) checkoutGroups() map[string]string {
	mi.mu.RLock()
	roots := make(map[string]string, len(mi.repos))
	for prefix, meta := range mi.repos {
		if meta == nil || prefix == "" || meta.RootPath == "" {
			continue
		}
		roots[prefix] = meta.RootPath
	}
	mi.mu.RUnlock()

	// Resolve outside the lock: ResolveWorktree touches the filesystem.
	byPrefix := make(map[string]string, len(roots))
	shared := make(map[string]int, len(roots))
	for prefix, root := range roots {
		group := resolvedMainRepo(root)
		if group == "" {
			continue
		}
		byPrefix[prefix] = group
		shared[group]++
	}
	for prefix, group := range byPrefix {
		if shared[group] < 2 {
			delete(byPrefix, prefix)
		}
	}
	return byPrefix
}
