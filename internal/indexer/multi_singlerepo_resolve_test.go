package indexer

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

func indexSingleRepoForTest(t *testing.T) (*MultiIndexer, string) {
	t.Helper()
	dir := setupRepoDir(t, "myrepo")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	return mi, dir
}

func indexTwoReposForTest(t *testing.T) (*MultiIndexer, string, string) {
	t.Helper()
	repoA := setupRepoDir(t, "repo-a")
	repoB := setupRepoDir(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	return mi, repoA, repoB
}

// Single-repo mode indexes nodes and file paths without a repo prefix
// while registering the repo's metadata under its real prefix. The empty
// prefix must therefore resolve to the lone repo — otherwise every node
// the single-repo indexer mints is unresolvable (no source reads, no
// savings recording, no editing by graph path).
func TestMultiIndexer_RepoRoot_EmptyPrefixResolvesLoneRepo(t *testing.T) {
	mi, dir := indexSingleRepoForTest(t)

	root, ok := mi.RepoRoot("")
	require.True(t, ok, "empty prefix must resolve when exactly one repo is tracked")
	assert.Equal(t, dir, root)

	// The real prefix keeps working.
	root, ok = mi.RepoRoot("myrepo")
	require.True(t, ok)
	assert.Equal(t, dir, root)

	// Unknown prefixes still miss.
	_, ok = mi.RepoRoot("nope")
	assert.False(t, ok)
}

func TestMultiIndexer_RepoRoot_EmptyPrefixAmbiguousInMultiRepo(t *testing.T) {
	mi, _, _ := indexTwoReposForTest(t)

	_, ok := mi.RepoRoot("")
	assert.False(t, ok, "empty prefix is ambiguous with two tracked repos")
}

func TestMultiIndexer_ResolveFilePath_UnprefixedSingleRepo(t *testing.T) {
	mi, dir := indexSingleRepoForTest(t)

	// Unprefixed path (the form single-repo nodes carry) anchors to the
	// lone repo root.
	assert.Equal(t, filepath.Join(dir, "main.go"), mi.ResolveFilePath("main.go"))

	// The prefixed form keeps working too.
	assert.Equal(t, filepath.Join(dir, "main.go"), mi.ResolveFilePath("myrepo/main.go"))
}

func TestMultiIndexer_ResolveFilePath_UnprefixedMultiRepoStaysEmpty(t *testing.T) {
	mi, _, _ := indexTwoReposForTest(t)

	assert.Empty(t, mi.ResolveFilePath("main.go"),
		"bare path with two tracked repos is ambiguous and must not resolve")
}

// TestMultiIndexer_RunPreEnrichResolve_BindsInboundCrossRepoEdge is the
// warm-restart completeness regression: on a partial restart where only the
// provider repo re-indexed, RunPreEnrichResolve scopes the same-repo master
// resolve to that repo but must still bind an unchanged consumer repo's inbound
// cross-repo reference — before the daemon flips ready. Scoping the cross-repo
// pass to the changed provider's own out-edges left the consumer-owned inbound
// edge unresolved for the whole enrichment window despite a "ready" daemon.
func TestMultiIndexer_RunPreEnrichResolve_BindsInboundCrossRepoEdge(t *testing.T) {
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	// Unchanged consumer repo-a: a call with no local definition (bare
	// unresolved), in a shared workspace so the cross-repo boundary is open.
	g.AddNode(&graph.Node{ID: "repo-a/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo-a/a.go", Language: "go", RepoPrefix: "repo-a", WorkspaceID: "shared"})
	// Changed provider repo-b: just added Foo, plus its file node for import
	// reachability.
	g.AddNode(&graph.Node{ID: "repo-b/b.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repo-b/b.go", Language: "go", RepoPrefix: "repo-b", WorkspaceID: "shared"})
	g.AddNode(&graph.Node{ID: "repo-b/b.go", Kind: graph.KindFile, Name: "repo-b/b.go", FilePath: "repo-b/b.go", Language: "go", RepoPrefix: "repo-b", WorkspaceID: "shared"})
	g.AddEdge(&graph.Edge{From: "repo-a/a.go", To: "repo-b/b.go", Kind: graph.EdgeImports, FilePath: "repo-a/a.go", Line: 1})

	inbound := &graph.Edge{From: "repo-a/a.go::Caller", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "repo-a/a.go", Line: 5}
	g.AddEdge(inbound)

	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	// Scoped warm restart: only the provider repo-b re-indexed.
	require.NoError(t, mi.RunPreEnrichResolve(context.Background(), map[string]struct{}{"repo-b": {}}, nil))

	assert.Equal(t, "repo-b/b.go::Foo", inbound.To,
		"pre-enrich resolve must bind the unchanged consumer's inbound edge into the changed provider before ready")
	assert.True(t, inbound.CrossRepo)
}
