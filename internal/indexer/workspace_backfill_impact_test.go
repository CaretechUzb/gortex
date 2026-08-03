package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

const workspaceBackfillTestPrefix = "repo"

type workspaceBackfillStoreFactory struct {
	name string
	open func(*testing.T) graph.Store
}

func workspaceBackfillStoreFactories() []workspaceBackfillStoreFactory {
	return []workspaceBackfillStoreFactory{
		{
			name: "graph",
			open: func(*testing.T) graph.Store { return graph.New() },
		},
		{
			name: "sqlite",
			open: func(t *testing.T) graph.Store {
				t.Helper()
				store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.db"))
				if err != nil {
					t.Fatalf("open SQLite graph: %v", err)
				}
				t.Cleanup(func() {
					if err := store.Close(); err != nil {
						t.Errorf("close SQLite graph: %v", err)
					}
				})
				return store
			},
		},
	}
}

func newWorkspaceBackfillFixture(
	t *testing.T,
	factory workspaceBackfillStoreFactory,
	workspaceConfig string,
	withContract bool,
) (*MultiIndexer, graph.Store, *contracts.Registry) {
	t.Helper()
	root := t.TempDir()
	if workspaceConfig != "" {
		if err := os.WriteFile(filepath.Join(root, ".gortex.yaml"), []byte(workspaceConfig), 0o600); err != nil {
			t.Fatalf("write workspace config: %v", err)
		}
	}
	configManager, err := config.NewConfigManager(filepath.Join(t.TempDir(), "missing-global.yaml"))
	if err != nil {
		t.Fatalf("new config manager: %v", err)
	}
	store := factory.open(t)
	store.AddNode(&graph.Node{
		ID:         workspaceBackfillTestPrefix + "/file.go::Function",
		Kind:       graph.KindFunction,
		Name:       "Function",
		FilePath:   workspaceBackfillTestPrefix + "/file.go",
		RepoPrefix: workspaceBackfillTestPrefix,
	})

	var registry *contracts.Registry
	indexers := make(map[string]*Indexer)
	if withContract {
		registry = contracts.NewRegistry()
		registry.Add(contracts.Contract{
			ID:         "contract",
			SymbolID:   workspaceBackfillTestPrefix + "/file.go::Function",
			FilePath:   workspaceBackfillTestPrefix + "/file.go",
			RepoPrefix: workspaceBackfillTestPrefix,
		})
		indexers[workspaceBackfillTestPrefix] = &Indexer{contractRegistry: registry}
	}

	multi := &MultiIndexer{
		graph: store,
		repos: map[string]*RepoMetadata{
			workspaceBackfillTestPrefix: {
				RepoPrefix: workspaceBackfillTestPrefix,
				RootPath:   root,
			},
		},
		indexers:  indexers,
		configMgr: configManager,
	}
	return multi, store, registry
}

func TestBackfillWorkspaceSlugsWithImpactPreservesLegacyCounts(t *testing.T) {
	cases := []struct {
		name                   string
		workspaceConfig        string
		wantWorkspace          string
		wantProject            string
		wantResolutionAffected int
	}{
		{
			name:                   "default slugs are physical only",
			wantWorkspace:          workspaceBackfillTestPrefix,
			wantProject:            workspaceBackfillTestPrefix,
			wantResolutionAffected: 0,
		},
		{
			name:                   "declared workspace changes eligibility",
			workspaceConfig:        "workspace: shared\nproject: application\n",
			wantWorkspace:          "shared",
			wantProject:            "application",
			wantResolutionAffected: 1,
		},
	}

	for _, factory := range workspaceBackfillStoreFactories() {
		factory := factory
		t.Run(factory.name, func(t *testing.T) {
			for _, tc := range cases {
				tc := tc
				t.Run(tc.name, func(t *testing.T) {
					legacyMulti, _, _ := newWorkspaceBackfillFixture(t, factory, tc.workspaceConfig, true)
					legacyNodes, legacyContracts := legacyMulti.BackfillWorkspaceSlugs()

					exactMulti, store, registry := newWorkspaceBackfillFixture(t, factory, tc.workspaceConfig, true)
					nodes, contractCount, resolutionAffected := exactMulti.BackfillWorkspaceSlugsWithImpact()
					if nodes != legacyNodes || contractCount != legacyContracts {
						t.Fatalf("exact counts = (%d, %d), legacy = (%d, %d)", nodes, contractCount, legacyNodes, legacyContracts)
					}
					if nodes != 1 || contractCount != 1 {
						t.Fatalf("backfill counts = (%d, %d), want (1, 1)", nodes, contractCount)
					}
					if resolutionAffected != tc.wantResolutionAffected {
						t.Fatalf("resolution affected = %d, want %d", resolutionAffected, tc.wantResolutionAffected)
					}

					node := store.GetNode(workspaceBackfillTestPrefix + "/file.go::Function")
					if node == nil {
						t.Fatal("backfilled node missing")
					}
					if node.WorkspaceID != tc.wantWorkspace || node.ProjectID != tc.wantProject {
						t.Fatalf("node slugs = (%q, %q), want (%q, %q)", node.WorkspaceID, node.ProjectID, tc.wantWorkspace, tc.wantProject)
					}
					allContracts := registry.All()
					if len(allContracts) != 1 {
						t.Fatalf("contracts = %d, want 1", len(allContracts))
					}
					if allContracts[0].WorkspaceID != tc.wantWorkspace || allContracts[0].ProjectID != tc.wantProject {
						t.Fatalf("contract slugs = (%q, %q), want (%q, %q)", allContracts[0].WorkspaceID, allContracts[0].ProjectID, tc.wantWorkspace, tc.wantProject)
					}

					secondNodes, secondContracts, secondAffected := exactMulti.BackfillWorkspaceSlugsWithImpact()
					if secondNodes != 0 || secondContracts != 0 || secondAffected != 0 {
						t.Fatalf("idempotent backfill = (%d, %d, %d), want zeros", secondNodes, secondContracts, secondAffected)
					}
				})
			}
		})
	}
}

type legacyWorkspaceSlugStore struct {
	graph.Store
	changed int
	seen    []graph.WorkspaceSlug
}

func (s *legacyWorkspaceSlugStore) BackfillWorkspaceSlugs(slugs []graph.WorkspaceSlug) int {
	s.seen = append(s.seen[:0], slugs...)
	return s.changed
}

func TestBackfillWorkspaceSlugsWithImpactUnknownBackendFailsClosed(t *testing.T) {
	root := t.TempDir()
	configManager, err := config.NewConfigManager(filepath.Join(t.TempDir(), "missing-global.yaml"))
	if err != nil {
		t.Fatalf("new config manager: %v", err)
	}
	legacy := &legacyWorkspaceSlugStore{Store: graph.New(), changed: 7}
	multi := &MultiIndexer{
		graph: legacy,
		repos: map[string]*RepoMetadata{
			workspaceBackfillTestPrefix: {
				RepoPrefix: workspaceBackfillTestPrefix,
				RootPath:   root,
			},
		},
		indexers:  make(map[string]*Indexer),
		configMgr: configManager,
	}

	nodes, contractCount, resolutionAffected := multi.BackfillWorkspaceSlugsWithImpact()
	if nodes != 7 || contractCount != 0 || resolutionAffected != 7 {
		t.Fatalf("legacy result = (%d, %d, %d), want (7, 0, 7)", nodes, contractCount, resolutionAffected)
	}
	if len(legacy.seen) != 1 || legacy.seen[0].RepoPrefix != workspaceBackfillTestPrefix {
		t.Fatalf("legacy backend received slugs = %+v", legacy.seen)
	}
}
