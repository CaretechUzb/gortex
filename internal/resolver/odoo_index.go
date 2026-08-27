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
func (ix odooIndex) lookup(g graph.Store, fromID, key string) string {
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
		if graph.SiblingCheckouts(g, askingRepo, repo) {
			continue
		}
		if best == "" || id < best {
			best = id
		}
	}
	return best
}

// odooDropSiblingCheckouts filters a fan-out target list down to the
// targets that are not duplicates living in a sibling checkout of the
// asking node's repository.
//
// The model binder fans out on purpose — several addons legitimately
// extend one `_name` — so its candidates cannot be collapsed to one the
// way odooIndex.lookup collapses a unique key. What they can be is
// deduplicated across checkouts: a second checkout contributes no new
// declaring class, only the same class again under a foreign prefix.
func odooDropSiblingCheckouts(g graph.Store, fromID string, targets []string) []string {
	if len(targets) == 0 || !graph.HasSiblingCheckouts(g) {
		return targets
	}
	askingRepo := graph.RepoPrefixOfID(fromID)
	kept := make([]string, 0, len(targets))
	for _, target := range targets {
		if graph.SiblingCheckouts(g, askingRepo, graph.RepoPrefixOfID(target)) {
			continue
		}
		kept = append(kept, target)
	}
	return kept
}
