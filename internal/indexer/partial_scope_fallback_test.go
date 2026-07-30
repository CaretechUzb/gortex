package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/semantic"
)

type partialScopeBatchProvider struct {
	language    string
	batchCalls  int
	singleCalls int
	fullCalls   int
	batches     [][]string
}

func (p *partialScopeBatchProvider) Name() string        { return "partial-scope-" + p.language }
func (p *partialScopeBatchProvider) Languages() []string { return []string{p.language} }
func (p *partialScopeBatchProvider) Available() bool     { return true }
func (p *partialScopeBatchProvider) Close() error        { return nil }
func (p *partialScopeBatchProvider) Enrich(graph.Store, string) (*semantic.EnrichResult, error) {
	p.fullCalls++
	return &semantic.EnrichResult{Provider: p.Name(), Language: p.language}, nil
}
func (p *partialScopeBatchProvider) EnrichFile(graph.Store, string, string) (*semantic.EnrichResult, error) {
	p.singleCalls++
	return &semantic.EnrichResult{Provider: p.Name(), Language: p.language}, nil
}
func (p *partialScopeBatchProvider) EnrichFiles(_ graph.Store, _ string, _ string, files []string) (*semantic.EnrichResult, error) {
	p.batchCalls++
	p.batches = append(p.batches, append([]string(nil), files...))
	return &semantic.EnrichResult{Provider: p.Name(), Language: p.language}, nil
}

func partialScopeRegistry() *parser.Registry {
	registry := parser.NewRegistry()
	languages.RegisterAll(registry)
	return registry
}

func TestPartialDeletionQueuesOnlySurvivingDependencyFrontier(t *testing.T) {
	root := t.TempDir()
	livePath := filepath.Join(root, "live.go")
	require.NoError(t, os.WriteFile(livePath, []byte("package sample\nfunc Use() {}\n"), 0o644))

	store := graph.New()
	store.AddBatch([]*graph.Node{
		{ID: "gone.go", Kind: graph.KindFile, Name: "gone.go", FilePath: "gone.go", Language: "go"},
		{ID: "gone.go::Gone", Kind: graph.KindFunction, Name: "Gone", FilePath: "gone.go", Language: "go"},
		{ID: "live.go", Kind: graph.KindFile, Name: "live.go", FilePath: "live.go", Language: "go"},
		{ID: "live.go::Use", Kind: graph.KindFunction, Name: "Use", FilePath: "live.go", Language: "go"},
	}, []*graph.Edge{{
		From: "live.go::Use", To: "gone.go::Gone", Kind: graph.EdgeCalls, FilePath: "live.go", Line: 2,
	}})
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(root)
	idx.contractRegistry = contracts.NewRegistry()
	idx.SetFileMtimes(map[string]int64{"gone.go": 1})

	result, err := idx.IncrementalReindexPaths(root, []string{filepath.Join(root, "gone.go")})
	require.NoError(t, err)
	require.Equal(t, 1, result.DeletedFileCount)
	files, full, _ := idx.deferredEnrichScope()
	assert.False(t, full, "one deletion must never promote semantic work to the repository")
	assert.ElementsMatch(t, []string{"gone.go", "live.go"}, files)
	assert.Nil(t, store.GetNode("gone.go::Gone"))
	assert.NotNil(t, store.GetNode("live.go::Use"))
}

func TestPartialNonGoReparseUsesOneLanguageBatchAndNoRepoPass(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "app.py")
	require.NoError(t, os.WriteFile(path, []byte("def value():\n    return 1\n"), 0o644))

	store := graph.New()
	idx := New(store, partialScopeRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRootPath(root)
	idx.deferGlobalPasses.Store(true)
	provider := &partialScopeBatchProvider{language: "python"}
	manager := semantic.NewManager(semantic.Config{
		Enabled: true,
		Providers: []semantic.ProviderConfig{{
			Name: provider.Name(), Languages: provider.Languages(), Priority: 1, Enabled: true,
		}},
	}, zap.NewNop())
	manager.RegisterProvider(provider)
	idx.SetSemanticManager(manager)
	require.NoError(t, idx.IndexFile(path))
	indexedInfo, err := os.Stat(path)
	require.NoError(t, err)
	idx.SetFileMtimes(map[string]int64{"app.py": indexedInfo.ModTime().UnixNano()})
	idx.pendingEnrich.Store(false)
	idx.deferredEnrichMu.Lock()
	idx.deferredEnrichFiles = nil
	idx.deferredEnrichFull = false
	idx.deferredEnrichMu.Unlock()
	*provider = partialScopeBatchProvider{language: "python"}

	future := time.Now().Add(time.Minute)
	require.NoError(t, os.WriteFile(path, []byte("def value(arg):\n    return arg\n"), 0o644))
	require.NoError(t, os.Chtimes(path, future, future))
	result, err := idx.IncrementalReindexPaths(root, []string{path})
	require.NoError(t, err)
	require.Equal(t, 1, result.StaleFileCount)
	require.Empty(t, result.FailedFiles)
	files, full, _ := idx.deferredEnrichScope()
	assert.False(t, full)
	assert.Equal(t, []string{"app.py"}, files)

	idx.runDeferredEnrich()
	require.Equal(t, 1, provider.batchCalls)
	assert.Zero(t, provider.singleCalls)
	assert.Zero(t, provider.fullCalls)
	require.Len(t, provider.batches, 1)
	assert.Equal(t, []string{"app.py"}, provider.batches[0])
	assert.False(t, idx.pendingEnrich.Load())
}

func TestPartialGoModRefreshMatchesFreshGlobalExtraction(t *testing.T) {
	root := t.TempDir()
	goModPath := filepath.Join(root, "go.mod")
	oldSource := "module example.com/app\n\nrequire example.com/old v1.0.0\n"
	newSource := "module example.com/app\n\nrequire example.com/new v2.0.0\n"
	require.NoError(t, os.WriteFile(goModPath, []byte(oldSource), 0o644))

	store := graph.New()
	store.AddNode(&graph.Node{ID: "go.mod", Kind: graph.KindFile, Name: "go.mod", FilePath: "go.mod", Language: "go"})
	idx := New(store, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRootPath(root)
	idx.extractContracts()
	idx.recordFileMtime("go.mod", goModPath)
	require.NotNil(t, store.GetNode("dep::example.com/old"))

	future := time.Now().Add(time.Minute)
	require.NoError(t, os.WriteFile(goModPath, []byte(newSource), 0o644))
	require.NoError(t, os.Chtimes(goModPath, future, future))
	result, err := idx.IncrementalReindexPaths(root, []string{goModPath})
	require.NoError(t, err)
	require.Equal(t, 1, result.StaleFileCount)
	_, full, _ := idx.deferredEnrichScope()
	assert.False(t, full, "go.mod refresh must remain an exact manifest frontier")
	assert.Nil(t, store.GetNode("dep::example.com/old"), "stale dependency contract survived")
	assert.NotNil(t, store.GetNode("dep::example.com/new"))

	expectedStore := graph.New()
	expectedStore.AddNode(&graph.Node{ID: "go.mod", Kind: graph.KindFile, Name: "go.mod", FilePath: "go.mod", Language: "go"})
	expected := New(expectedStore, newTestRegistry(), config.Default().Index, zap.NewNop())
	expected.SetRootPath(root)
	expected.extractContracts()
	require.NotNil(t, expected.contractRegistry)
	require.NotNil(t, idx.contractRegistry)
	assert.True(t, contractSetsEqual(expected.contractRegistry.All(), idx.contractRegistry.All()),
		"bounded go.mod refresh diverged from fresh global contract extraction")
}

func TestCrossFileContractSourceExpandsOnlyExistingContractFiles(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "mount.py"), []byte("app.include_router(users)\n"), 0o644))
	reg := contracts.NewRegistry()
	reg.Add(contracts.Contract{ID: "http::GET::/a", FilePath: "a.py", RepoPrefix: "repo"})
	reg.Add(contracts.Contract{ID: "http::GET::/b", FilePath: "b.py", RepoPrefix: "repo"})
	idx := New(graph.New(), partialScopeRegistry(), config.IndexConfig{}, zap.NewNop())
	idx.SetRootPath(root)
	idx.SetRepoPrefix("repo")

	frontier := idx.expandIncrementalContractFrontier([]string{"repo/mount.py"}, reg)
	assert.ElementsMatch(t, []string{"repo/mount.py", "a.py", "b.py"}, frontier)
}
