package graph

// WorkspaceSlug is one repository-to-boundary mapping used by warm-start
// backfill. Empty values preserve the existing node column.
type WorkspaceSlug struct {
	RepoPrefix string
	Workspace  string
	Project    string
}

// WorkspaceSlugBackfillResult separates physical row changes from changes
// that can alter cross-workspace resolution. Cross-repo resolution treats an
// empty WorkspaceID as RepoPrefix, so filling that exact value is semantic
// preservation; ProjectID is not consulted by that resolver.
type WorkspaceSlugBackfillResult struct {
	Changed            int
	ResolutionAffected int
}

// WorkspaceSlugBackfiller is an optional set-oriented mutation capability.
// Implementations must fill only empty workspace/project columns, process all
// supplied repositories as one bounded operation, and return changed rows.
type WorkspaceSlugBackfiller interface {
	BackfillWorkspaceSlugs(slugs []WorkspaceSlug) int
}

// WorkspaceSlugImpactBackfiller additionally reports whether a fill changed
// the effective workspace boundary. Callers that lack this capability must
// conservatively treat every changed row as resolution-affecting.
type WorkspaceSlugImpactBackfiller interface {
	BackfillWorkspaceSlugsWithImpact(slugs []WorkspaceSlug) WorkspaceSlugBackfillResult
}

// WorkspaceSlugChangesResolution reports the only legacy fill that can change
// cross-workspace eligibility. An empty current workspace already falls back
// to repoPrefix, so assigning that same value is a non-semantic stamp.
func WorkspaceSlugChangesResolution(repoPrefix, currentWorkspace, newWorkspace string) bool {
	return currentWorkspace == "" && newWorkspace != "" && newWorkspace != repoPrefix
}

// BackfillWorkspaceSlugs updates resident nodes under shard write locks. It
// walks the native repo buckets once and never creates a graph-wide snapshot.
func (g *Graph) BackfillWorkspaceSlugs(slugs []WorkspaceSlug) int {
	return g.BackfillWorkspaceSlugsWithImpact(slugs).Changed
}

// BackfillWorkspaceSlugsWithImpact updates resident nodes under shard write
// locks and distinguishes physical stamps from effective workspace changes.
func (g *Graph) BackfillWorkspaceSlugsWithImpact(slugs []WorkspaceSlug) WorkspaceSlugBackfillResult {
	byRepo := make(map[string]WorkspaceSlug, len(slugs))
	for _, slug := range slugs {
		if slug.RepoPrefix == "" || (slug.Workspace == "" && slug.Project == "") {
			continue
		}
		byRepo[slug.RepoPrefix] = slug
	}
	if len(byRepo) == 0 {
		return WorkspaceSlugBackfillResult{}
	}

	var result WorkspaceSlugBackfillResult
	for _, shard := range g.shards {
		shard.mu.Lock()
		for repoPrefix, slug := range byRepo {
			for _, node := range shard.byRepo[repoPrefix] {
				if node == nil {
					continue
				}
				dirty := false
				if node.WorkspaceID == "" && slug.Workspace != "" {
					// Builtin stubs are target-only synthetic sentinels. The
					// cross-repository resolver never considers KindBuiltin as a
					// function, method, file, or import candidate, so filling its
					// physical boundary columns cannot change resolution.
					if node.Kind != KindBuiltin && WorkspaceSlugChangesResolution(node.RepoPrefix, node.WorkspaceID, slug.Workspace) {
						result.ResolutionAffected++
					}
					node.WorkspaceID = slug.Workspace
					dirty = true
				}
				if node.ProjectID == "" && slug.Project != "" {
					node.ProjectID = slug.Project
					dirty = true
				}
				if dirty {
					result.Changed++
				}
			}
		}
		shard.mu.Unlock()
	}
	return result
}

var _ WorkspaceSlugBackfiller = (*Graph)(nil)
var _ WorkspaceSlugImpactBackfiller = (*Graph)(nil)
