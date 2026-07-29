package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

func TestSQLiteReplaceContractOwnersKeepsSharedIDOwnersRepositoryScoped(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	store.AddBatch([]*graph.Node{
		{ID: "repo-a::handler", Kind: graph.KindFunction, Name: "handlerA", FilePath: "repo-a/a.go", RepoPrefix: "repo-a"},
		{ID: "repo-b::handler", Kind: graph.KindFunction, Name: "handlerB", FilePath: "repo-b/b.go", RepoPrefix: "repo-b"},
		{ID: "repo-a::service", Kind: graph.KindFunction, Name: "service", FilePath: "repo-a/a.go", RepoPrefix: "repo-a"},
		{ID: "shared-contract", Kind: graph.KindContract, Name: "shared-contract", FilePath: "repo-b/b.go", RepoPrefix: "repo-b"},
		{ID: "di-token", Kind: graph.KindInterface, Name: "di-token", FilePath: "repo-a/a.go", RepoPrefix: "repo-a"},
	}, []*graph.Edge{
		{From: "repo-a::handler", To: "shared-contract", Kind: graph.EdgeProvides, FilePath: "repo-a/a.go", Line: 10},
		{From: "repo-b::handler", To: "shared-contract", Kind: graph.EdgeConsumes, FilePath: "repo-b/b.go", Line: 20},
		{From: "repo-a::service", To: "di-token", Kind: graph.EdgeProvides, FilePath: "repo-a/a.go", Line: 30},
	})

	result, err := store.ReplaceContractOwners(graph.ContractOwnerReplacement{
		RepoPrefix:     "repo-a",
		FilePaths:      []string{"repo-a/a.go"},
		TouchedNodeIDs: []string{"shared-contract"},
		Nodes: []*graph.Node{
			{ID: "shared-contract", Kind: graph.KindContract, Name: "shared-contract", FilePath: "repo-a/a.go", RepoPrefix: "repo-a"},
		},
		Edges: []*graph.Edge{
			{From: "repo-a::handler", To: "shared-contract", Kind: graph.EdgeConsumes, FilePath: "repo-a/a.go", Line: 11},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, result.EdgesRemoved)
	assertSQLiteOwnerEdge(t, store.GetOutEdges("repo-a::handler"), graph.EdgeConsumes, "shared-contract", 11)
	assertSQLiteOwnerEdge(t, store.GetOutEdges("repo-b::handler"), graph.EdgeConsumes, "shared-contract", 20)
	assertSQLiteOwnerEdge(t, store.GetOutEdges("repo-a::service"), graph.EdgeProvides, "di-token", 30)

	_, err = store.ReplaceContractOwners(graph.ContractOwnerReplacement{
		RepoPrefix:     "repo-a",
		FilePaths:      []string{"repo-a/a.go"},
		TouchedNodeIDs: []string{"shared-contract"},
	})
	require.NoError(t, err)
	assert.NotNil(t, store.GetNode("shared-contract"))
	assertSQLiteOwnerEdge(t, store.GetOutEdges("repo-b::handler"), graph.EdgeConsumes, "shared-contract", 20)
	assertSQLiteOwnerEdge(t, store.GetOutEdges("repo-a::service"), graph.EdgeProvides, "di-token", 30)

	_, err = store.ReplaceContractOwners(graph.ContractOwnerReplacement{
		RepoPrefix:     "repo-b",
		FilePaths:      []string{"repo-b/b.go"},
		TouchedNodeIDs: []string{"shared-contract"},
	})
	require.NoError(t, err)
	assert.Nil(t, store.GetNode("shared-contract"))
	assert.Empty(t, store.GetOutEdges("repo-b::handler"))
}

func TestSQLiteReplaceContractOwnersRollsBackDeleteAndInsertTogether(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "graph.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	store.AddBatch([]*graph.Node{
		{ID: "repo-a::handler", Kind: graph.KindFunction, Name: "handler", FilePath: "repo-a/a.go", RepoPrefix: "repo-a"},
		{ID: "shared-contract", Kind: graph.KindContract, Name: "shared-contract", FilePath: "repo-a/a.go", RepoPrefix: "repo-a"},
	}, []*graph.Edge{
		{From: "repo-a::handler", To: "shared-contract", Kind: graph.EdgeProvides, FilePath: "repo-a/a.go", Line: 10},
	})
	_, err = store.writerDB.Exec(`CREATE TRIGGER fail_new_contract
BEFORE INSERT ON nodes WHEN NEW.id = 'new-contract'
BEGIN SELECT RAISE(ABORT, 'forced replacement failure'); END`)
	require.NoError(t, err)

	_, err = store.ReplaceContractOwners(graph.ContractOwnerReplacement{
		RepoPrefix:     "repo-a",
		FilePaths:      []string{"repo-a/a.go"},
		TouchedNodeIDs: []string{"shared-contract"},
		Nodes: []*graph.Node{
			{ID: "shared-contract", Kind: graph.KindContract, Name: "shared-contract", QualName: "contract.shared", FilePath: "repo-a/a.go", RepoPrefix: "repo-a"},
			{ID: "new-contract", Kind: graph.KindContract, Name: "new-contract", QualName: "contract.new", FilePath: "repo-a/a.go", RepoPrefix: "repo-a"},
		},
		Edges: []*graph.Edge{
			{From: "repo-a::handler", To: "shared-contract", Kind: graph.EdgeConsumes, FilePath: "repo-a/a.go", Line: 11},
		},
	})
	require.Error(t, err)
	assert.NotNil(t, store.GetNode("shared-contract"))
	assert.Nil(t, store.GetNode("new-contract"))
	assertSQLiteOwnerEdge(t, store.GetOutEdges("repo-a::handler"), graph.EdgeProvides, "shared-contract", 10)
}

func assertSQLiteOwnerEdge(t *testing.T, edges []*graph.Edge, kind graph.EdgeKind, to string, line int) {
	t.Helper()
	require.Len(t, edges, 1)
	assert.Equal(t, kind, edges[0].Kind)
	assert.Equal(t, to, edges[0].To)
	assert.Equal(t, line, edges[0].Line)
}
