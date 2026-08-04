package resolver

import "github.com/zzet/gortex/internal/graph"

// ResolveFilesAndIncoming runs one scoped cross-repository pass for a complete
// mutation batch. The frontier contains unresolved edges emitted by the changed
// files and unresolved incoming stub buckets for the symbols they define. Both
// directions share one resolver lock and one build of the directory,
// dependency, reachability, and lookup indexes.
func (cr *CrossRepoResolver) ResolveFilesAndIncoming(filePaths []string) *CrossRepoStats {
	return cr.ResolveMutationFiles(filePaths, filePaths)
}

// ResolveMutationFiles separates files that can create or bind unresolved
// edges from files whose resolved base edges only need cross-repository
// materialization. Keeping both roles under one lock still shares the expensive
// workspace indexes without feeding every semantic edge back through resolution.
func (cr *CrossRepoResolver) ResolveMutationFiles(resolutionFiles, materializationFiles []string) *CrossRepoStats {
	return cr.ResolveMutationFrontiers(resolutionFiles, materializationFiles, materializationFiles)
}

// ResolveMutationFrontiers preserves the distinct edge-source and changed-
// definition roles through candidate selection.
func (cr *CrossRepoResolver) ResolveMutationFrontiers(resolutionFiles, edgeSourceFiles, definitionFiles []string) *CrossRepoStats {
	stats := &CrossRepoStats{ByRepo: make(map[string]int)}
	if cr == nil || cr.graph == nil || len(resolutionFiles) == 0 && len(edgeSourceFiles) == 0 && len(definitionFiles) == 0 {
		return stats
	}

	cr.mu.Lock()
	defer cr.mu.Unlock()

	if len(resolutionFiles) > 0 {
		frontier := collectIncrementalFileFrontier(cr.graph, resolutionFiles, nil)
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
	}
	DetectCrossRepoEdgesForMutationFiles(cr.graph, edgeSourceFiles, definitionFiles)
	return stats
}

// DetectCrossRepoEdgesForFiles materializes the cross-repo layer only for base
// edges incident to nodes in the exact changed-file frontier. Inspecting both
// incoming and outgoing edges covers unchanged callers rebound to a changed
// target as well as new calls emitted by the changed source file.
func DetectCrossRepoEdgesForFiles(g graph.Store, filePaths []string) int {
	return DetectCrossRepoEdgesForMutationFiles(g, filePaths, filePaths)
}

// DetectCrossRepoEdgesForMutationFiles materializes resolved base edges using
// the narrow query shape for each mutation role.
func DetectCrossRepoEdgesForMutationFiles(g graph.Store, edgeSourceFiles, definitionFiles []string) int {
	if g == nil || len(edgeSourceFiles) == 0 && len(definitionFiles) == 0 {
		return 0
	}
	return materializeCrossRepoCandidates(g, crossRepoCandidatesForMutationFiles(g, edgeSourceFiles, definitionFiles))
}
