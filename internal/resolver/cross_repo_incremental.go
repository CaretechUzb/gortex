package resolver

import "github.com/zzet/gortex/internal/graph"

// ResolveFilesAndIncoming runs one scoped cross-repository pass for a complete
// mutation batch. The frontier contains unresolved edges emitted by the changed
// files and unresolved incoming stub buckets for the symbols they define. Both
// directions share one resolver lock and one build of the directory,
// dependency, reachability, and lookup indexes.
func (cr *CrossRepoResolver) ResolveFilesAndIncoming(filePaths []string) *CrossRepoStats {
	stats := &CrossRepoStats{ByRepo: make(map[string]int)}
	if cr == nil || cr.graph == nil || len(filePaths) == 0 {
		return stats
	}

	cr.mu.Lock()
	defer cr.mu.Unlock()

	frontier := collectIncrementalFileFrontier(cr.graph, filePaths, nil)
	pending := make([]*graph.Edge, 0, len(frontier.pending))
	seen := make(map[graph.EdgeIdentity]struct{}, len(frontier.pending))
	for _, edge := range frontier.pending {
		if edge == nil || !graph.IsUnresolvedTarget(edge.To) {
			continue
		}
		identity := graph.EdgeIdentityFor(edge)
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		pending = append(pending, edge)
	}
	if len(pending) > 0 {
		stats = cr.resolveScopedLocked(pending)
	}
	DetectCrossRepoEdgesForFiles(cr.graph, frontier.paths)
	return stats
}

// DetectCrossRepoEdgesForFiles materializes the cross-repo layer only for base
// edges incident to nodes in the exact changed-file frontier. Inspecting both
// incoming and outgoing edges covers unchanged callers rebound to a changed
// target as well as new calls emitted by the changed source file.
func DetectCrossRepoEdgesForFiles(g graph.Store, filePaths []string) int {
	if g == nil || len(filePaths) == 0 {
		return 0
	}
	return materializeCrossRepoCandidates(g, crossRepoCandidatesForFiles(g, filePaths))
}
