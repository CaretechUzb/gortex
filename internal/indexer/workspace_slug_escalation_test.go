package indexer

import (
	"context"
	"iter"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

type workspaceSlugImpactStore struct {
	graph.Store
	result          graph.WorkspaceSlugBackfillResult
	unresolvedScans int
}

func (s *workspaceSlugImpactStore) BackfillWorkspaceSlugs([]graph.WorkspaceSlug) int {
	return s.result.Changed
}

func (s *workspaceSlugImpactStore) BackfillWorkspaceSlugsWithImpact([]graph.WorkspaceSlug) graph.WorkspaceSlugBackfillResult {
	return s.result
}

func (s *workspaceSlugImpactStore) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	s.unresolvedScans++
	return func(func(*graph.Edge) bool) {}
}

func TestRunPreEnrichResolveFilesEscalatesOnlyForEffectiveWorkspaceChange(t *testing.T) {
	tests := []struct {
		name      string
		result    graph.WorkspaceSlugBackfillResult
		wantScans int
	}{
		{
			name:      "physical stamps preserve RepoPrefix fallback",
			result:    graph.WorkspaceSlugBackfillResult{Changed: 4},
			wantScans: 0,
		},
		{
			name:      "effective workspace boundary changed",
			result:    graph.WorkspaceSlugBackfillResult{Changed: 1, ResolutionAffected: 1},
			wantScans: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := &workspaceSlugImpactStore{Store: graph.New(), result: tt.result}
			mi := &MultiIndexer{
				graph:     store,
				repos:     make(map[string]*RepoMetadata),
				indexers:  make(map[string]*Indexer),
				configMgr: newTestConfigManager(t),
			}

			require.NoError(t, mi.RunPreEnrichResolveFiles(context.Background(), nil, nil))
			require.Equal(t, tt.wantScans, store.unresolvedScans)
		})
	}
}
