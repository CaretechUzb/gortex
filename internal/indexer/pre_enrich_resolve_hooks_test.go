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

// onComputeDone carves the readiness boundary out of the pre-enrich resolve
// stage: the daemon flips queryable there. It must fire exactly once, and it
// must not change what the stage resolves. (Enrichment applies deliberately
// get no earlier hook: they park until the whole stage — cross-repo included
// — returns, because an apply's multi-minute ResolveMutex holds starve the
// cross-repo pass when admitted mid-stage.)
func TestRunPreEnrichResolveFiresComputeDoneHook(t *testing.T) {
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	g.AddNode(&graph.Node{ID: "repo-a/a.go::Caller", Kind: graph.KindFunction, Name: "Caller", FilePath: "repo-a/a.go", Language: "go", RepoPrefix: "repo-a", WorkspaceID: "shared"})
	g.AddNode(&graph.Node{ID: "repo-b/b.go::Foo", Kind: graph.KindFunction, Name: "Foo", FilePath: "repo-b/b.go", Language: "go", RepoPrefix: "repo-b", WorkspaceID: "shared"})
	g.AddNode(&graph.Node{ID: "repo-b/b.go", Kind: graph.KindFile, Name: "repo-b/b.go", FilePath: "repo-b/b.go", Language: "go", RepoPrefix: "repo-b", WorkspaceID: "shared"})
	g.AddEdge(&graph.Edge{From: "repo-a/a.go", To: "repo-b/b.go", Kind: graph.EdgeImports, FilePath: "repo-a/a.go", Line: 1})
	inbound := &graph.Edge{From: "repo-a/a.go::Caller", To: "unresolved::Foo", Kind: graph.EdgeCalls, FilePath: "repo-a/a.go", Line: 5}
	g.AddEdge(inbound)

	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	var order []string
	require.NoError(t, mi.RunPreEnrichResolve(context.Background(), nil,
		func() { order = append(order, "compute_done") }))

	assert.Equal(t, []string{"compute_done"}, order,
		"queryable must be declarable exactly once, before the stage returns")
	assert.Equal(t, "repo-b/b.go::Foo", inbound.To,
		"the hooks must not change what the stage resolves")
}
