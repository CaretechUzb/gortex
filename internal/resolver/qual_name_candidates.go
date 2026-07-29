package resolver

import "github.com/zzet/gortex/internal/graph"

// lowestIDQualNameCandidate makes every policy tier deterministic without
// treating qualified-name order as semantic identity.
func lowestIDQualNameCandidate(candidates []*graph.Node, accept func(*graph.Node) bool) *graph.Node {
	var picked *graph.Node
	for _, candidate := range candidates {
		if candidate == nil || !accept(candidate) {
			continue
		}
		if picked == nil || candidate.ID < picked.ID {
			picked = candidate
		}
	}
	return picked
}

// pickResolverQualNameCandidate handles the normal resolver, where an exact
// caller-repository instance is authoritative. A same-workspace instance wins
// among duplicates in that repository. A single foreign candidate preserves
// the historical cross-repo import behavior; several foreign candidates are
// ambiguous and must be deferred to the cross-repository resolver.
func pickResolverQualNameCandidate(candidates []*graph.Node, callerRepo, callerWorkspace string) (*graph.Node, bool) {
	if candidate := lowestIDQualNameCandidate(candidates, func(n *graph.Node) bool {
		return n.RepoPrefix == callerRepo && candidateWorkspaceID(n) == callerWorkspace
	}); candidate != nil {
		return candidate, false
	}
	if candidate := lowestIDQualNameCandidate(candidates, func(n *graph.Node) bool {
		return n.RepoPrefix == callerRepo
	}); candidate != nil {
		return candidate, false
	}

	var only *graph.Node
	for _, candidate := range candidates {
		if candidate == nil {
			continue
		}
		if only != nil {
			return nil, true
		}
		only = candidate
	}
	return only, false
}
