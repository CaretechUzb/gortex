package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// countFTSRowidRows returns the symbol_fts_rowid rows mapped to nodeID.
func countFTSRowidRows(t *testing.T, path, nodeID string) int {
	t.Helper()
	var n int
	withRawDB(t, path, func(db *sql.DB) {
		require.NoError(t, db.QueryRow(
			`SELECT COUNT(*) FROM symbol_fts_rowid WHERE node_id = ?`, nodeID).Scan(&n))
	})
	return n
}

// TestOpenPurgesLegacyCoverageSpellings is the upgrade proof for the
// coverage-domain path-spelling purge. Stores written on Windows before
// the builders preserved the extractor's path spelling hold todo/fixture
// nodes and licensed_as / owns / generated_by / depends_on_module /
// annotated edges keyed by the forward-slash twin of the native
// backslash spelling. Nothing evicts those rows (eviction is
// spelling-exact), so a versioned migration removes them: per-file
// artifact nodes and coverage edges selectively by kind + FilePath
// spelling, shared targets only once nothing references them. Every
// native-spelled row must survive untouched.
func TestOpenPurgesLegacyCoverageSpellings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		nativeA = `r/src\a.go`
		nativeB = `r/src\b.go`
		legacyA = `r/src/a.go`

		nativeTodo  = nativeA + `::todo:5`
		legacyTodo  = legacyA + `::todo:3`
		legacyFix   = `r/testdata/x.json`
		licMIT      = `r/license::MIT`
		licGPL      = `r/license::GPL-3.0`
		teamCore    = `r/team::core`
		moduleX     = `r/module::go::example.com/x@v1`
		nativeMod   = `r/sub\go.mod`
		legacyMod   = `r/sub/go.mod`
		genExternal = `r/external::protoc`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		// Native file nodes: prove the store keys paths with backslashes.
		{ID: nativeA, Kind: graph.KindFile, Name: "a.go", FilePath: nativeA, RepoPrefix: "r"},
		{ID: nativeB, Kind: graph.KindFile, Name: "b.go", FilePath: nativeB, RepoPrefix: "r"},
		// Native todo: must survive.
		{ID: nativeTodo, Kind: graph.KindTodo, Name: "todo:5", FilePath: nativeA, RepoPrefix: "r"},
		// Legacy per-file artifacts: must be purged.
		{ID: legacyTodo, Kind: graph.KindTodo, Name: "todo:3", FilePath: legacyA, RepoPrefix: "r"},
		{ID: legacyFix, Kind: graph.KindFixture, Name: "x.json", FilePath: legacyFix, RepoPrefix: "r"},
		// Shared target anchored to the LEGACY spelling but still
		// referenced natively by b: node must survive, anchor and all.
		{ID: licMIT, Kind: graph.KindLicense, Name: "MIT", FilePath: legacyA, RepoPrefix: "r"},
		// Shared target whose only reference is legacy: orphaned after
		// the edge purge, so the node goes too.
		{ID: licGPL, Kind: graph.KindLicense, Name: "GPL-3.0", FilePath: legacyA, RepoPrefix: "r"},
		// Team referenced by a non-coverage authored edge: the legacy
		// owns edge is purged but the node must survive.
		{ID: teamCore, Kind: graph.KindTeam, Name: "core", FilePath: legacyA, RepoPrefix: "r"},
		// Shared target anchored NATIVELY with one legacy + one native
		// edge: node and native edge survive.
		{ID: moduleX, Kind: graph.KindModule, Name: "x", FilePath: nativeMod, RepoPrefix: "r"},
	}, []*graph.Edge{
		// Native rows: every one must survive.
		{From: nativeA, To: nativeTodo, Kind: graph.EdgeAnnotated, FilePath: nativeA, Line: 5},
		{From: nativeB, To: licMIT, Kind: graph.EdgeLicensedAs, FilePath: nativeB},
		{From: teamCore, To: nativeB, Kind: graph.EdgeAuthored, FilePath: nativeB},
		{From: nativeMod, To: moduleX, Kind: graph.EdgeDependsOnModule, FilePath: nativeMod, Line: 2},
		// Legacy rows: every one must be purged.
		{From: legacyA, To: legacyTodo, Kind: graph.EdgeAnnotated, FilePath: legacyA, Line: 3},
		{From: legacyA, To: licMIT, Kind: graph.EdgeLicensedAs, FilePath: legacyA},
		{From: legacyA, To: licGPL, Kind: graph.EdgeLicensedAs, FilePath: legacyA},
		{From: teamCore, To: legacyA, Kind: graph.EdgeOwns, FilePath: legacyA},
		{From: legacyA, To: genExternal, Kind: graph.EdgeGeneratedBy, FilePath: legacyA},
		{From: legacyMod, To: moduleX, Kind: graph.EdgeDependsOnModule, FilePath: legacyMod, Line: 2},
	})
	// Legacy artifact nodes were FTS-indexed by the old binary; the purge
	// must clear their search rows so no ghost hits outlive the nodes.
	require.NoError(t, s.UpsertSymbolFTS(legacyTodo, "stale marker"))
	require.NoError(t, s.UpsertSymbolFTS(nativeTodo, "live marker"))
	require.NoError(t, s.Close())

	// Simulate a store written before the purge shipped.
	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)

	// Purged rows.
	require.Nil(t, s2.GetNode(legacyTodo), "legacy todo node must be purged")
	require.Nil(t, s2.GetNode(legacyFix), "legacy fixture node must be purged")
	require.Nil(t, s2.GetNode(licGPL), "orphaned shared license must be removed")
	require.Empty(t, s2.GetOutEdges(legacyA), "no legacy-spelled coverage edge may survive")
	// Survivors.
	require.NotNil(t, s2.GetNode(nativeTodo), "native todo node must survive")
	require.NotNil(t, s2.GetNode(licMIT), "shared license anchored to a legacy path stays while referenced")
	require.NotNil(t, s2.GetNode(teamCore), "team referenced by an authored edge stays")
	require.NotNil(t, s2.GetNode(moduleX), "natively referenced module stays")

	inMIT := s2.GetInEdges(licMIT)
	require.Len(t, inMIT, 1, "exactly b's native licensed_as edge remains on MIT")
	require.Equal(t, nativeB, inMIT[0].From)

	outTeam := s2.GetOutEdges(teamCore)
	require.Len(t, outTeam, 1, "only the authored edge remains on the team")
	require.Equal(t, graph.EdgeAuthored, outTeam[0].Kind)

	inMod := s2.GetInEdges(moduleX)
	require.Len(t, inMod, 1, "exactly the native depends_on_module edge remains")
	require.Equal(t, nativeMod, inMod[0].From)

	outA := s2.GetOutEdges(nativeA)
	require.Len(t, outA, 1, "the native annotated edge survives")
	require.Equal(t, graph.EdgeAnnotated, outA[0].Kind)

	require.NoError(t, s2.Close())
	require.Zero(t, countFTSRowidRows(t, path, legacyTodo), "purged node's FTS rows must go with it")
	require.NotZero(t, countFTSRowidRows(t, path, nativeTodo), "surviving node keeps its FTS rows")
}

// TestOpenPurgesLegacyCoverageSpellingsSingleRepo covers the unprefixed
// store shape: no repo prefix means the whole FilePath is the path
// portion, so any forward slash marks the legacy twin on a
// backslash-keyed store.
func TestOpenPurgesLegacyCoverageSpellingsSingleRepo(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		nativeA    = `src\a.go`
		legacyA    = `src/a.go`
		nativeTodo = nativeA + `::todo:5`
		legacyTodo = legacyA + `::todo:3`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: nativeA, Kind: graph.KindFile, Name: "a.go", FilePath: nativeA},
		{ID: nativeTodo, Kind: graph.KindTodo, Name: "todo:5", FilePath: nativeA},
		{ID: legacyTodo, Kind: graph.KindTodo, Name: "todo:3", FilePath: legacyA},
	}, []*graph.Edge{
		{From: nativeA, To: nativeTodo, Kind: graph.EdgeAnnotated, FilePath: nativeA, Line: 5},
		{From: legacyA, To: legacyTodo, Kind: graph.EdgeAnnotated, FilePath: legacyA, Line: 3},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	require.Nil(t, s2.GetNode(legacyTodo), "legacy todo node must be purged")
	require.Empty(t, s2.GetOutEdges(legacyA), "legacy annotated edge must be purged")
	require.NotNil(t, s2.GetNode(nativeTodo), "native todo node must survive")
	require.Len(t, s2.GetOutEdges(nativeA), 1, "native annotated edge must survive")
}

// TestPurgeLegacyCoverageSpellingsIsIdempotent runs the step twice on
// one connection: the second pass must find nothing left to remove and
// must not trip over the temp tables the first pass created.
func TestPurgeLegacyCoverageSpellingsIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		nativeA    = `r/src\a.go`
		legacyA    = `r/src/a.go`
		legacyTodo = legacyA + `::todo:3`
		lic        = `r/license::MIT`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: nativeA, Kind: graph.KindFile, Name: "a.go", FilePath: nativeA, RepoPrefix: "r"},
		{ID: legacyTodo, Kind: graph.KindTodo, Name: "todo:3", FilePath: legacyA, RepoPrefix: "r"},
		{ID: lic, Kind: graph.KindLicense, Name: "MIT", FilePath: legacyA, RepoPrefix: "r"},
	}, []*graph.Edge{
		{From: legacyA, To: legacyTodo, Kind: graph.EdgeAnnotated, FilePath: legacyA, Line: 3},
		{From: legacyA, To: lic, Kind: graph.EdgeLicensedAs, FilePath: legacyA},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		nodesAfter := func() int {
			var n int
			require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM nodes`).Scan(&n))
			return n
		}
		for pass := 1; pass <= 2; pass++ {
			tx, err := db.Begin()
			require.NoError(t, err)
			require.NoError(t, purgeLegacyCoverageSpellings(tx), "pass %d", pass)
			require.NoError(t, tx.Commit())
		}
		require.Equal(t, 1, nodesAfter(), "only the native file node survives both passes")
	})
}

// TestOpenLeavesPosixCoverageRowsUntouched pins the guard: on a store
// with no backslash-keyed paths every row IS the native spelling, so
// the purge must not run at all. Without the guard the migration would
// eat every coverage artifact on every POSIX store.
func TestOpenLeavesPosixCoverageRowsUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")

	const (
		fileA = `r/src/a.go`
		todoA = `r/src/a.go::todo:3`
		lic   = `r/license::MIT`
	)

	s, err := Open(path)
	require.NoError(t, err)
	s.AddBatch([]*graph.Node{
		{ID: fileA, Kind: graph.KindFile, Name: "a.go", FilePath: fileA, RepoPrefix: "r"},
		{ID: todoA, Kind: graph.KindTodo, Name: "todo:3", FilePath: fileA, RepoPrefix: "r"},
		{ID: lic, Kind: graph.KindLicense, Name: "MIT", FilePath: fileA, RepoPrefix: "r"},
	}, []*graph.Edge{
		{From: fileA, To: todoA, Kind: graph.EdgeAnnotated, FilePath: fileA, Line: 3},
		{From: fileA, To: lic, Kind: graph.EdgeLicensedAs, FilePath: fileA},
	})
	require.NoError(t, s.Close())

	withRawDB(t, path, func(db *sql.DB) {
		_, err := db.Exec(`PRAGMA user_version = 12`)
		require.NoError(t, err, "reset to the pre-purge version")
	})

	s2, err := Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s2.Close() })

	require.NotNil(t, s2.GetNode(todoA), "POSIX todo node must survive")
	require.NotNil(t, s2.GetNode(lic), "POSIX license node must survive")
	require.Len(t, s2.GetOutEdges(fileA), 2, "both POSIX coverage edges must survive")
}
