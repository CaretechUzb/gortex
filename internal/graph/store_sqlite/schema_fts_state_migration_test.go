package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

func TestOpenV10AddsSymbolFTSStateWithoutRebuildingCorpus(t *testing.T) {
	if currentSchemaVersion != 11 {
		t.Fatalf("currentSchemaVersion = %d, want 11 for the symbol FTS state migration", currentSchemaVersion)
	}
	var v11 *schemaMigration
	for i := range schemaMigrations {
		if schemaMigrations[i].version == 11 {
			v11 = &schemaMigrations[i]
			break
		}
	}
	if v11 == nil || v11.rebuild || v11.inPlace == nil {
		t.Fatalf("v11 migration = %+v, want a registered in-place step", v11)
	}

	path := filepath.Join(t.TempDir(), "v10.sqlite")
	seed, err := Open(path)
	if err != nil {
		t.Fatalf("create current store: %v", err)
	}

	const (
		symbolID = "repo/legacy.go::LegacyWidget"
		callerID = "repo/caller.go::UseLegacyWidget"
	)
	seed.AddBatch([]*graph.Node{
		{ID: symbolID, Kind: graph.KindType, Name: "LegacyWidget", FilePath: "repo/legacy.go", RepoPrefix: "repo"},
		{ID: callerID, Kind: graph.KindFunction, Name: "UseLegacyWidget", FilePath: "repo/caller.go", RepoPrefix: "repo"},
	}, []*graph.Edge{{
		From: callerID, To: symbolID, Kind: graph.EdgeCalls, FilePath: "repo/caller.go", Line: 7,
	}})
	if err := seed.UpsertSymbolFTS(symbolID, "migration sentinel"); err != nil {
		t.Fatalf("seed symbol FTS: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	// Recreate the exact v10 shape: the graph and native FTS corpus exist, but
	// the per-repository normalization marker introduced by v11 does not.
	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(`DROP TABLE symbol_fts_state`); err != nil {
			t.Fatalf("drop post-v10 marker table: %v", err)
		}
		if _, err := db.Exec(`PRAGMA user_version = 10`); err != nil {
			t.Fatalf("stamp v10: %v", err)
		}
	})

	migrated, err := Open(path)
	if err != nil {
		t.Fatalf("open v10 store: %v", err)
	}
	defer migrated.Close()

	if migrated.NeedsRebuild() {
		t.Fatal("additive v10->v11 state-table migration requested a rebuild")
	}
	if version, err := readUserVersion(migrated.db); err != nil || version != currentSchemaVersion {
		t.Fatalf("migrated user_version = %d (err %v), want %d", version, err, currentSchemaVersion)
	}
	var stateTables int
	if err := migrated.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'symbol_fts_state'`).Scan(&stateTables); err != nil {
		t.Fatalf("probe symbol_fts_state: %v", err)
	}
	if stateTables != 1 {
		t.Fatalf("symbol_fts_state table count = %d, want 1", stateTables)
	}
	assertFTSNormalizationState(t, migrated, "repo", "", false)

	if got := migrated.NodeCount(); got != 2 {
		t.Fatalf("node count after migration = %d, want 2", got)
	}
	if got := migrated.EdgeCount(); got != 1 {
		t.Fatalf("edge count after migration = %d, want 1", got)
	}
	hits, err := migrated.SearchSymbols("sentinel", 10)
	if err != nil {
		t.Fatalf("search preserved v10 FTS corpus: %v", err)
	}
	if len(hits) != 1 || hits[0].NodeID != symbolID {
		t.Fatalf("preserved FTS hits = %+v, want %q", hits, symbolID)
	}
}
