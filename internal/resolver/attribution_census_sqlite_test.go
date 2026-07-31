package resolver

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

func TestPostBareAttributionCensusSQLiteDoesNotDecodeCorruptMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt-meta.sqlite")
	store, err := store_sqlite.Open(path)
	require.NoError(t, err)
	store.AddEdge(&graph.Edge{
		From: "arg", To: "unresolved::Target", Kind: graph.EdgeArgOf,
		FilePath: "pkg/file.go", Line: 10, Meta: map[string]any{"payload": "valid"},
	})
	require.NoError(t, store.Close())

	raw, err := sql.Open("sqlite", path)
	require.NoError(t, err)
	_, err = raw.Exec("UPDATE edges SET meta = ?", []byte{0xff})
	require.NoError(t, err)
	require.NoError(t, raw.Close())

	reopened, err := store_sqlite.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = reopened.Close() })
	census, stats := New(reopened).buildPostBareAttributionCensus(defaultAttributionCensusLimits)
	require.NotNil(t, census)
	defer census.close()
	assert.Empty(t, stats.fallbackReason)
	assert.Equal(t, 1, stats.rowsScanned)
	assert.Equal(t, 1, stats.dataflowCandidates)
}

func TestPostBareAttributionCensusSQLiteMatchesLegacyPasses(t *testing.T) {
	legacyStore, err := store_sqlite.Open(filepath.Join(t.TempDir(), "legacy.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = legacyStore.Close() })
	sharedStore, err := store_sqlite.Open(filepath.Join(t.TempDir(), "shared.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = sharedStore.Close() })

	sources := addPostBareAttributionSQLiteFixture(legacyStore)
	addPostBareAttributionSQLiteFixture(sharedStore)

	legacy := New(legacyStore)
	legacy.bindDataflowCalleeRefs()
	legacy.bindGenericParamRefs()

	shared := New(sharedStore)
	census, stats := shared.buildPostBareAttributionCensus(defaultAttributionCensusLimits)
	require.NotNil(t, census)
	require.Empty(t, stats.fallbackReason)
	assert.Equal(t, 4, stats.rowsScanned)
	assert.Equal(t, 2, stats.dataflowCandidates)
	assert.Equal(t, 1, stats.genericCandidates)
	shared.bindDataflowCalleeRefsFromCensus(census)
	census.releaseDataflow()
	shared.bindGenericParamRefsFromCensus(census)
	census.close()

	for source, want := range sources {
		legacyEdges := legacyStore.GetOutEdges(source)
		require.Len(t, legacyEdges, 1, "legacy source %s", source)
		sharedEdges := sharedStore.GetOutEdges(source)
		require.Len(t, sharedEdges, 1, "shared source %s", source)
		assert.Equal(t, want, legacyEdges[0].To, "legacy source %s", source)
		assert.Equal(t, want, sharedEdges[0].To, "shared source %s", source)
	}
}

func addPostBareAttributionSQLiteFixture(store graph.Store) map[string]string {
	file := "pkg/file.go"
	owner := file + "::Owner"
	functionTarget := file + "::Do"
	methodTarget := file + "::Worker.Run"
	tparamTarget := owner + "#tparam:T"
	argFunction := owner + "#arg:0"
	argMethod := owner + "#arg:1"
	genericUse := owner + "#local:value"
	store.AddBatch([]*graph.Node{
		{ID: owner, Kind: graph.KindFunction, Name: "Owner", FilePath: file, Language: "go"},
		{ID: functionTarget, Kind: graph.KindFunction, Name: "Do", FilePath: file, Language: "go"},
		{ID: methodTarget, Kind: graph.KindMethod, Name: "Run", FilePath: file, Language: "go"},
		{ID: tparamTarget, Kind: graph.KindGenericParam, Name: "T", FilePath: file, Language: "go"},
	}, []*graph.Edge{
		{From: owner, To: methodTarget, Kind: graph.EdgeCalls, FilePath: file, Line: 20},
		{From: argFunction, To: "unresolved::Do", Kind: graph.EdgeArgOf, FilePath: file, Line: 10},
		{From: argMethod, To: "unresolved::*.Run", Kind: graph.EdgeValueFlow, FilePath: file, Line: 20},
		{From: genericUse, To: "unresolved::T", Kind: graph.EdgeTypedAs, FilePath: file, Line: 30},
	})
	return map[string]string{
		argFunction: functionTarget,
		argMethod:   methodTarget,
		genericUse:  tparamTarget,
	}
}
