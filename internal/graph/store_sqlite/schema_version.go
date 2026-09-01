package store_sqlite

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Schema versioning for the graph store.
//
// Unlike the sidecar (which holds irreplaceable user data and must migrate in
// place), the graph store is a DERIVED CACHE: every row is reconstructable by
// re-indexing the source. So the cheapest *always-correct* reaction to a schema
// change an old on-disk DB can't satisfy is to drop the file and let the daemon
// rebuild it on the next index. A migration may therefore declare rebuild=true
// instead of writing an in-place transform that would have to re-derive the new
// data from source anyway. In-place steps remain the cheap path for purely
// mechanical changes (a new index, a denormalisation, a column with a
// computable default) that spare a large repo a multi-minute reindex.
//
// The whole mechanism keys off SQLite's built-in PRAGMA user_version, read on
// Open before schemaSQL runs. There is no separate version table.
//
// Concurrency: the daemon holds an exclusive flock on <store>.lock around Open
// (see serverstack.NewSharedServer), so reading the version, wiping the file,
// and stamping it cannot race another process. That is why — unlike the
// sidecar — this path needs no BEGIN IMMEDIATE / busy-loop handling.

// currentSchemaVersion is the version a fully-reconciled store reports via
// PRAGMA user_version. Bump it whenever schemaSQL's typed-column shape or an
// index changes in a way an old on-disk DB would not already have, and append a
// matching schemaMigrations entry describing how to bring an older store
// forward (in place, or by rebuild).
const currentSchemaVersion = 14

// schemaMigration is one forward step. Exactly one strategy applies:
//   - rebuild=true: the change introduces structure/data that can only come
//     from re-indexing the source; an older store is wiped and rebuilt.
//   - inPlace!=nil: the change is mechanically derivable from the existing
//     store and is applied in a transaction with no reindex.
//
// Steps are append-only and ascending; never edit or renumber a shipped one.
// Any inPlace step must be idempotent (IF NOT EXISTS / ADD COLUMN guarded).
type schemaMigration struct {
	version int
	name    string
	inPlace func(tx *sql.Tx) error
	rebuild bool
}

// schemaMigrations is the ordered, forward-only registry. Version 1 is the
// implicit baseline (no entry): a v1 store is reconciled entirely by schemaSQL's
// idempotent CREATE ... IF NOT EXISTS plus ensureNodeColumns, so any
// pre-versioning database baseline-stamps to v1 without a rebuild. Append
// entries for version 2 and up as the schema evolves.
var schemaMigrations = []schemaMigration{
	{version: 2, name: "dedupe fn-value placeholder edges", inPlace: dedupeFnValuePlaceholderEdges},
	// Versions through v2 wrote node updates with INSERT OR REPLACE. REPLACE
	// has delete semantics and can invalidate incident-edge integrity when
	// foreign-key enforcement is enabled by a host/connection. Deleted edges
	// cannot be reconstructed from the remaining graph rows, so this is an
	// explicit source-reindex boundary rather than a misleading in-place fix.
	{version: 3, name: "restore topology after node replace writes", rebuild: true},
	{version: 4, name: "add normalized analysis generations", inPlace: createAnalysisGenerationTables},
	{version: 5, name: "backfill flat graph ownership, provenance, and clone corpus", inPlace: backfillSyntheticNodeRepoPrefixes},
	{version: 6, name: "compact resolver edge indexes", inPlace: compactResolverEdgeIndexes},
	// Single-repo (unprefixed) mode is gone: every repo's nodes now carry
	// its prefix. A store written before the flip holds a solo repo's
	// file-backed nodes under repo_prefix='', which nothing else can reach
	// or evict, and the first post-upgrade warmup writes a full prefixed
	// copy beside them. Purge the old population here — see the function
	// for why in-place beats rebuild and why global externals survive.
	{version: 7, name: "purge unprefixed solo-repo rows", inPlace: purgeUnprefixedRepoRows},
	{version: 8, name: "allow duplicate qualified names", inPlace: relaxNodeQualNameUniqueness},
	{version: 9, name: "drop unused semantic pending index", inPlace: dropUnusedSemanticPendingIndex},
	// Vector ownership and chunk-parent identity cannot be reconstructed from
	// the legacy (node_id, dims, vec) rows for every ID shape. Rebuild only the
	// derived vector sidecar rather than discarding otherwise-valid topology.
	{version: 10, name: "rebuild vector corpus ownership and parents", inPlace: rebuildVectorCorpusSchema},
	{version: 11, name: "add symbol FTS normalization state", inPlace: createSymbolFTSNormalizationStateTable},
	{version: 12, name: "normalize dir column separators", inPlace: normalizeDirColumnSeparators},
	// v13 carries BOTH sides' work, because both sides minted a v13.
	//
	// This branch developed its readiness migration as v13 in parallel with
	// main's coverage-spelling purge, so the merge had to reconcile two
	// different steps claiming one number. They are merged into the single v13
	// they both claim rather than renumbering ours to 14, which would have left
	// main's purge permanently unrun on every store this branch built.
	//
	// Standing rule, and the reason renumbering was rejected in both
	// directions: this branch's currentSchemaVersion must stay EQUAL to main's.
	// Our work lives inside the number main already shipped, so whatever main
	// numbers next (14) is strictly greater than what stores built here are
	// stamped, and pendingBetween selects it. Stamping our stores 14/15 would
	// carry them past main's next migration, which pendingBetween would then
	// skip permanently.
	//
	// Consequence, accepted deliberately: planSchemaMigrationWith treats
	// `stored > current` as "written by a newer build" and WIPES the store, so a
	// store this branch already stamped 14 is rebuilt from source on first open
	// after the merge. That is the cost of collapsing the number, and it is
	// correct — such a store carries a schema this registry no longer describes.
	//
	// Known gap, recorded rather than hidden: a store MAIN stamped 13 is
	// `stored == current` and runs nothing, so it never receives the readiness
	// seeding or the unstamp. schemaSQL still creates the readiness tables (it
	// runs before the registry), so such a store is structurally sound, but its
	// repositories read "never derived" until their next real derive, and any
	// synthetic-namespace ownership the old graph.StubRepoPrefix wrote is left
	// standing. Waiting does not close it either: pendingBetween(13, 14) selects
	// v14 alone, so main's next migration reconciles nothing here. A rebuild
	// closes both halves; the repo's next real derive closes the readiness rows
	// only, and the unstamp stays unrun. Do not mint a migration of our own to
	// close it — see the standing rule above. The v14 below is MAIN's, adopted
	// verbatim in the merge; it is not a number minted here to close this gap.
	{version: 13, name: "purge legacy coverage spellings and add per-repo readiness state", inPlace: migrateV13},
	//
	// Resolved 2026-09-01 in the upstream merge: main's next migration did
	// arrive as v14 (purge unresolved derived tests edges) and is adopted below
	// verbatim, so currentSchemaVersion is now 14 — equal to main's, which is
	// exactly what the standing rule permits: go higher only once main is
	// merged in and its number is already ours. A store this fork stamped 13
	// runs v14 alone, which is right, because it already carries both halves
	// of v13.
	{version: 14, name: "purge unresolved derived tests edges", inPlace: purgeUnresolvedTestsEdges},
}

// migrateV13 runs main's coverage purge and this branch's readiness setup as
// one step, in dependency order.
//
// Purge first: it deletes legacy artifact rows, and the readiness seed reads
// repo_index_state to decide which repositories get a legacy=1 row, so seeding
// first would anchor rows the purge then strips.
//
// Unstamp last: it DELETEs from repo_graph_gen, which createReadinessStateTables
// is what creates and seeds. Reversing them would delete from a table that does
// not exist yet on a pre-v13 store.
func migrateV13(tx *sql.Tx) error {
	if err := purgeLegacyCoverageSpellings(tx); err != nil {
		return err
	}
	if err := createReadinessStateTables(tx); err != nil {
		return err
	}
	return unstampReservedRepoPrefixes(tx)
}

// unstampReservedRepoPrefixes clears the repo ownership that was stamped onto
// nodes living inside a synthetic namespace, and drops the anchor rows those
// nodes earned.
//
// The two statements are ordered, not independent. A prefix earns its
// repo_graph_gen row by OWNING A NODE (see bumpRepoGensTx), so deleting the row
// while a node still claimed the prefix would let the next mutation recreate
// it. Clearing ownership first is what makes the delete final.
//
// Dropping an anchor row is safe here in a way it is not in general —
// bumpRepoGensTx deliberately keeps advancing a real repo's row after its last
// file is evicted, because that eviction is precisely when the anchor must
// move. These prefixes are not repositories and never were, so there is no
// later mutation whose readiness verdict the row could have carried.
//
// Both statements seek: nodes_by_repo covers the UPDATE's predicate and
// repo_graph_gen is keyed by prefix. Neither scans, which matters — this runs
// inside Open, and a scan of a multi-gigabyte nodes table would stall every
// daemon start behind it.
//
// Idempotent: a second run matches nothing. Deliberately narrow, too — it says
// nothing about a node whose prefix is a real repository, including the
// `<repo>::module::…` rows the v5 backfill wrote correctly.
func unstampReservedRepoPrefixes(tx *sql.Tx) error {
	reserved := graph.ReservedIDNamespaces()
	args := make([]any, 0, len(reserved))
	for _, ns := range reserved {
		args = append(args, ns)
	}
	list := "(?" + strings.Repeat(", ?", len(reserved)-1) + ")"

	if _, err := tx.Exec(
		`UPDATE nodes SET repo_prefix = '' WHERE repo_prefix IN `+list, args...); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`DELETE FROM repo_graph_gen WHERE repo_prefix IN `+list, args...); err != nil {
		return err
	}
	return nil
}

// createReadinessStateTables is the readiness half of the merged v13 step.
// schemaSQL owns the canonical fresh-store definitions; this idempotent step
// brings an existing store forward and seeds the rows readiness needs to tell
// "never derived" apart from "derived before anything recorded derives".
//
// Every pre-v13 repo is seeded legacy=1. Derive completion was not persisted
// anywhere before this table -- the passes only logged -- so stamping a
// completion here would be inventing data about work this store cannot
// confirm happened. Legacy rows deliberately render "unknown" until the next
// real derive overwrites them.
func createReadinessStateTables(tx *sql.Tx) error {
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS repo_graph_gen (
		repo_prefix TEXT    PRIMARY KEY,
		gen         INTEGER NOT NULL DEFAULT 0,
		content_gen INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS derive_state (
		repo_prefix         TEXT PRIMARY KEY,
		derived_gen         INTEGER NOT NULL DEFAULT 0,
		derived_content_gen INTEGER NOT NULL DEFAULT 0,
		derived_sha         TEXT    NOT NULL DEFAULT '',
		derived_at          INTEGER NOT NULL DEFAULT 0,
		pass_version        INTEGER NOT NULL DEFAULT 0,
		config_hash         TEXT    NOT NULL DEFAULT '',
		scoped              INTEGER NOT NULL DEFAULT 0,
		legacy              INTEGER NOT NULL DEFAULT 0
	) WITHOUT ROWID`); err != nil {
		return err
	}
	// SQLite has no ADD COLUMN IF NOT EXISTS, and a duplicate ADD is a hard
	// error that would roll the entire migration transaction back on a re-run.
	//
	// The three content_gen columns are added inside this step rather than as a
	// migration of their own: they correct this step's own anchor, and the only
	// stores carrying the earlier shape are development builds of this branch.
	// A dev store stamped 14 is not the problem — `stored > current` now wipes
	// it on open. A dev store stamped 13 is `stored == current`, re-runs
	// nothing, and is exactly why each ADD is guarded individually and why
	// schemaSQL's CREATE TABLE IF NOT EXISTS cannot be relied on to deliver
	// them.
	//
	// This reasoning originally read "before v13 has shipped". That premise
	// died in the merge: main shipped its OWN v13 (the coverage-spelling purge)
	// while this branch was out, and the two were folded into one v13 rather
	// than renumbered. See the note on the migration table.
	for _, col := range []struct{ table, name, ddl string }{
		{"enrichment_state", "gen", "gen INTEGER NOT NULL DEFAULT 0"},
		{"enrichment_state", "content_gen", "content_gen INTEGER NOT NULL DEFAULT 0"},
		{"repo_graph_gen", "content_gen", "content_gen INTEGER NOT NULL DEFAULT 0"},
		{"derive_state", "derived_content_gen", "derived_content_gen INTEGER NOT NULL DEFAULT 0"},
	} {
		var present int
		if err := tx.QueryRow(
			`SELECT COUNT(*) FROM pragma_table_info(?) WHERE name = ?`, col.table, col.name,
		).Scan(&present); err != nil {
			return err
		}
		if present > 0 {
			continue
		}
		if _, err := tx.Exec(
			`ALTER TABLE ` + col.table + ` ADD COLUMN ` + col.ddl,
		); err != nil {
			return err
		}
	}
	// OR IGNORE, not a bare INSERT: schemaSQL runs before the migration steps,
	// so on a fresh store both tables already exist, and a re-run must not
	// collide with -- or overwrite -- rows a real derive has since written.
	if _, err := tx.Exec(`INSERT OR IGNORE INTO repo_graph_gen (repo_prefix, gen)
		SELECT repo_prefix, 1 FROM repo_index_state`); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO derive_state (repo_prefix, legacy)
		SELECT repo_prefix, 1 FROM repo_index_state`)
	return err
}

// purgeUnresolvedTestsEdges removes derived EdgeTests rows whose target is
// an unresolved stub, in both spellings (`unresolved::X` and the multi-repo
// `<repo>::unresolved::X` COPY-rewrite form). The test-linkage pass cloned
// them from unresolved calls before the emission guard existed; stripped of
// the call's receiver evidence they are naked stubs the resolver now
// refuses to bind, new emission never re-creates them, and warm startup may
// skip file-scoped reconciliation entirely — so an old store keeps paying
// their resolver-scan cost forever without this explicit purge. Idempotent
// and bounded to the tests kind: pending calls and resolved projections are
// untouched.
func purgeUnresolvedTestsEdges(tx *sql.Tx) error {
	_, err := tx.Exec(`DELETE FROM edges WHERE kind = 'tests'
		AND (to_id LIKE 'unresolved::%' OR to_id LIKE '%::unresolved::%')`)
	return err
}

// normalizeDirColumnSeparators rebuilds the two generated dir columns whose
// pre-v12 expressions trimmed at '/' only. Stored paths keep the writing
// platform's native separators below the repo prefix, so a Windows-written
// store collapsed both dirs to the repo prefix and the Go receiver-rebind
// join degenerated from "same package dir" to "same repo". A generated
// column cannot be redefined in place: drop the index over file_dir, drop
// and re-add both columns with the current DDL, and recreate the index from
// the shared always-live set so its DDL cannot drift. Idempotent — a re-run
// re-adds the same expressions, and the existence guards tolerate a store
// where a column is already absent or already current.
func normalizeDirColumnSeparators(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP INDEX IF EXISTS nodes_go_receiver_type`); err != nil {
		return err
	}
	for _, col := range []struct{ table, name, ddl string }{
		{"nodes", "file_dir", fileDirColumnDDL},
		{"edges", "member_receiver_dir", memberReceiverDirColumnDDL},
	} {
		var count int
		q := fmt.Sprintf(`SELECT COUNT(*) FROM pragma_table_xinfo('%s') WHERE name = ?`, col.table)
		if err := tx.QueryRow(q, col.name).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			if _, err := tx.Exec(`ALTER TABLE ` + col.table + ` DROP COLUMN ` + col.name); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`ALTER TABLE ` + col.table + ` ADD COLUMN ` + col.ddl); err != nil {
			return err
		}
	}
	for _, idx := range bulkAlwaysLiveIndexes {
		if idx.name == "nodes_go_receiver_type" {
			_, err := tx.Exec(idx.ddl)
			return err
		}
	}
	return fmt.Errorf("nodes_go_receiver_type missing from bulkAlwaysLiveIndexes")
}

// createSymbolFTSNormalizationStateTable is the explicit v11 migration for
// existing stores. schemaSQL owns the canonical fresh-store definition; this
// idempotent step makes the additive table part of the versioned contract.
func createSymbolFTSNormalizationStateTable(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS symbol_fts_state (
		repo_prefix TEXT PRIMARY KEY,
		normalization TEXT NOT NULL DEFAULT ''
	) WITHOUT ROWID`)
	return err
}

// dropUnusedSemanticPendingIndex removes an experimental index for a query
// shape that was never shipped. It was dense throughout parsing (semantic_type
// starts NULL), adding write and cold-finalization cost without a consumer.
func dropUnusedSemanticPendingIndex(tx *sql.Tx) error {
	_, err := tx.Exec(`DROP INDEX IF EXISTS nodes_semantic_pending`)
	return err
}

// rebuildVectorCorpusSchema intentionally discards only the durable vector
// sidecar. Legacy rows do not carry repository ownership or chunk parents, so
// retaining them would make per-repository replacement and warm de-chunking
// unsound. The graph topology remains intact and the next embedding pass
// repopulates this derived cache.
func rebuildVectorCorpusSchema(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP TABLE IF EXISTS vectors`); err != nil {
		return err
	}
	_, err := tx.Exec(vectorTableSQL)
	return err
}

// relaxNodeQualNameUniqueness removes the historical assumption that a
// language-level qualified name is a global graph identity. Resource manifests,
// forks, worktrees and overload-like constructs may legitimately repeat one.
// The replacement keeps the same name, key and partial predicate so existing
// point and batch lookups retain their indexed plans.
func relaxNodeQualNameUniqueness(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP INDEX IF EXISTS nodes_by_qual`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX nodes_by_qual ON nodes(qual_name) WHERE qual_name <> ''`)
	return err
}

// compactResolverEdgeIndexes removes the one-shot global Go receiver index and
// replaces the dense Boolean unresolved index with an unresolved-only partial
// index. Both changes are mechanically derivable from existing flat/generated
// columns, so a populated v5 store upgrades transactionally without reindexing
// source. A CREATE failure rolls the DROP operations back with the migration.
func compactResolverEdgeIndexes(tx *sql.Tx) error {
	if _, err := tx.Exec(`DROP INDEX IF EXISTS edges_go_member_receiver`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP INDEX IF EXISTS edges_by_unresolved`); err != nil {
		return err
	}
	_, err := tx.Exec(`CREATE INDEX edges_by_unresolved ON edges(is_unresolved) WHERE is_unresolved = 1`)
	return err
}

// backfillSyntheticNodeRepoPrefixes repairs rows written before synthetic
// resolver nodes consistently carried Node.RepoPrefix, then promotes legacy
// edge semantic_source values through a bounded Meta migration. Repo-scoped stub IDs
// have the form <repo>::<stub-kind>::..., so ownership is derivable without
// reading Meta or source files. Shared legacy dep:: / external:: IDs start
// directly with their kind and deliberately remain global.
//
// This is a single set-oriented UPDATE that runs once while upgrading v4 to
// v5. Keeping it in the schema migration avoids both an N+1 rewrite and a
// warm-start scan on every subsequent Open.
func backfillSyntheticNodeRepoPrefixes(tx *sql.Tx) error {
	if err := ensureCloneCorpusColumns(tx); err != nil {
		return err
	}
	if _, err := tx.Exec(`
UPDATE nodes
SET repo_prefix = substr(id, 1, instr(id, '::') - 1)
WHERE repo_prefix = ''
  AND instr(id, '::') > 0
  AND (
      substr(id, instr(id, '::') + 2) LIKE 'module::%'
      OR substr(id, instr(id, '::') + 2) LIKE 'stdlib::%'
      OR substr(id, instr(id, '::') + 2) LIKE 'builtin::%'
      OR substr(id, instr(id, '::') + 2) LIKE 'external_call::%'
  )`); err != nil {
		return err
	}
	return backfillEdgeSemanticSources(tx)
}

// The v4→v5 edge provenance migration is deliberately bounded in two
// dimensions. A page never retains more than pageRows candidate blobs or
// pageBytes of their encoded Meta (apart from one individually oversized row),
// while updateRows keeps each VALUES statement below SQLite's conservative
// 999-host-parameter ceiling (300 * 3 = 900).
//
// The SQL query first searches the BLOB for the literal key bytes. All three
// codecs accepted by decodeMeta — flat, JSON, and legacy gob — store map keys as
// their UTF-8 bytes, so this is a no-false-negative prefilter. A value or nested
// key may produce a harmless false positive; Go still performs the authoritative
// top-level type check before rewriting a row.
const (
	edgeSemanticSourceMigrationPageRows   = 4096
	edgeSemanticSourceMigrationPageBytes  = int64(8 << 20)
	edgeSemanticSourceMigrationUpdateRows = 300
	edgeSemanticSourceMetaMarker          = "semantic_source"
)

type edgeSemanticSourceMigrationRow struct {
	id     int64
	source string
	meta   []byte
}

type edgeSemanticSourceMigrationLimits struct {
	pageRows   int
	pageBytes  int64
	updateRows int
}

var defaultEdgeSemanticSourceMigrationLimits = edgeSemanticSourceMigrationLimits{
	pageRows:   edgeSemanticSourceMigrationPageRows,
	pageBytes:  edgeSemanticSourceMigrationPageBytes,
	updateRows: edgeSemanticSourceMigrationUpdateRows,
}

// edgeSemanticSourceMigrationStats is intentionally package-private test
// instrumentation. It makes the warm-up cost contract observable without a
// driver-specific query hook or production logging.
type edgeSemanticSourceMigrationStats struct {
	PageQueries      int
	RowsDecoded      int
	BytesDecoded     int64
	RowsUpdated      int
	UpdateStatements int
	MaxPageRows      int
	MaxPageBytes     int64
}

// backfillEdgeSemanticSources lifts Meta["semantic_source"] into its flat edge
// column without ever materialising the edge corpus. Pages advance by the
// stable integer edge id, and each page is rewritten with one VALUES-driven
// UPDATE in the caller's migration transaction. A failed page rolls the whole
// migration back; rerunning is idempotent because already-promoted rows no
// longer satisfy semantic_source IS NULL.
func backfillEdgeSemanticSources(tx *sql.Tx) error {
	_, err := backfillEdgeSemanticSourcesWithLimits(tx, defaultEdgeSemanticSourceMigrationLimits)
	return err
}

func backfillEdgeSemanticSourcesWithLimits(
	tx *sql.Tx,
	limits edgeSemanticSourceMigrationLimits,
) (edgeSemanticSourceMigrationStats, error) {
	var stats edgeSemanticSourceMigrationStats
	if limits.pageRows <= 0 || limits.pageBytes <= 0 || limits.updateRows <= 0 {
		return stats, fmt.Errorf("invalid edge semantic_source migration limits: rows=%d bytes=%d updates=%d",
			limits.pageRows, limits.pageBytes, limits.updateRows)
	}

	var afterID int64
	for {
		stats.PageQueries++
		rows, err := tx.Query(`
SELECT id, meta
FROM edges
WHERE id > ?
  AND semantic_source IS NULL
  AND meta IS NOT NULL
  AND instr(CAST(meta AS BLOB), ?) > 0
ORDER BY id
LIMIT ?`, afterID, []byte(edgeSemanticSourceMetaMarker), limits.pageRows)
		if err != nil {
			return stats, err
		}

		read := 0
		var pageBytes int64
		byteLimited := false
		updates := make([]edgeSemanticSourceMigrationRow, 0, min(limits.pageRows, limits.updateRows))
		for rows.Next() {
			var id int64
			var blob sql.RawBytes
			if err := rows.Scan(&id, &blob); err != nil {
				_ = rows.Close()
				return stats, err
			}
			// Do not advance afterID until this row has actually been decoded. If
			// accepting it would cross the byte cap, close this result and let
			// the next keyset query return the same row as its first candidate.
			// One row larger than the cap is accepted alone to guarantee progress.
			blobBytes := int64(len(blob))
			if read > 0 && pageBytes+blobBytes > limits.pageBytes {
				byteLimited = true
				break
			}
			read++
			afterID = id
			pageBytes += blobBytes
			stats.RowsDecoded++
			stats.BytesDecoded += blobBytes
			meta, err := decodeMeta(blob)
			if err != nil {
				_ = rows.Close()
				return stats, fmt.Errorf("decode edge %d meta: %w", id, err)
			}
			source, ok := meta["semantic_source"].(string)
			if !ok {
				if pageBytes >= limits.pageBytes {
					byteLimited = true
					break
				}
				continue
			}
			delete(meta, edgeSemanticSourceMetaMarker)
			remaining, err := encodeMeta(meta)
			if err != nil {
				_ = rows.Close()
				return stats, fmt.Errorf("encode edge %d meta: %w", id, err)
			}
			updates = append(updates, edgeSemanticSourceMigrationRow{id: id, source: source, meta: remaining})
			if pageBytes >= limits.pageBytes {
				byteLimited = true
				break
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return stats, err
		}
		if err := rows.Close(); err != nil {
			return stats, err
		}

		if read > stats.MaxPageRows {
			stats.MaxPageRows = read
		}
		if pageBytes > stats.MaxPageBytes {
			stats.MaxPageBytes = pageBytes
		}
		for start := 0; start < len(updates); start += limits.updateRows {
			end := min(start+limits.updateRows, len(updates))
			if err := applyEdgeSemanticSourceMigrationUpdates(tx, updates[start:end]); err != nil {
				return stats, err
			}
			stats.UpdateStatements++
			stats.RowsUpdated += end - start
		}
		if !byteLimited && read < limits.pageRows {
			return stats, nil
		}
	}
}

func applyEdgeSemanticSourceMigrationUpdates(tx *sql.Tx, updates []edgeSemanticSourceMigrationRow) error {
	if len(updates) == 0 {
		return nil
	}
	var values strings.Builder
	args := make([]any, 0, len(updates)*3)
	for i, update := range updates {
		if i > 0 {
			values.WriteByte(',')
		}
		values.WriteString("(?,?,?)")
		args = append(args, update.id, update.source, update.meta)
	}
	query := `WITH updates(id, semantic_source, meta) AS (VALUES ` + values.String() + `)
UPDATE edges AS e
SET semantic_source = u.semantic_source, meta = u.meta
FROM updates AS u
WHERE e.id = u.id AND e.semantic_source IS NULL`
	_, err := tx.Exec(query, args...)
	return err
}

// createAnalysisGenerationTables is the explicit v4 in-place migration.
// schemaSQL runs first and is intentionally idempotent, so this is a no-op on
// fresh stores and a defensive create on older stores opened by migration
// tests or future alternate open paths.
func createAnalysisGenerationTables(tx *sql.Tx) error {
	if _, err := tx.Exec(analysisGenerationSchemaSQL); err != nil {
		return err
	}
	// Builds used during development briefly created a blob-only cache under
	// schema v3. It was never released; remove the artifact instead of carrying
	// a conversion or compatibility API into v4.
	_, err := tx.Exec(`DROP TABLE IF EXISTS analysis_cache`)
	return err
}

// dedupeFnValuePlaceholderEdges collapses duplicate function-as-value gate
// placeholder edges (graph.FnValuePlaceholderMarker, `unresolved::fnvalue::
// <name>`) to one row per (from_id, to_id), keeping the MIN(id) survivor. The
// capture path now dedups per (from, name) before it emits, but stores written
// earlier accumulated one placeholder per call site — a live store held
// millions — and EdgesWithUnresolvedTarget plus the resolver's terminal
// reconcile materialised every one on each warm restart, the dominant warmup
// heap transient this step drains. The keep set is small (tens of thousands of
// distinct pairs), so the NOT IN materialisation is cheap; the ph filter rides
// the edges_by_to(to_id) range for the bare form and the is_unresolved index for
// the multi-repo infix form. Idempotent: a second run finds no duplicates. Freed
// pages return to the freelist and are reused by later writes; the file itself
// shrinks only under a manual VACUUM, deliberately out of scope for a derived
// cache that reclaims the space on its own.
func dedupeFnValuePlaceholderEdges(tx *sql.Tx) error {
	_, err := tx.Exec(`
WITH ph AS (
    SELECT id, from_id, to_id FROM edges
    WHERE (to_id >= 'unresolved::fnvalue::' AND to_id < 'unresolved::fnvalue:;')
       OR (is_unresolved = 1 AND to_id LIKE '%::unresolved::fnvalue::%')
), keep AS (
    SELECT MIN(id) AS id FROM ph GROUP BY from_id, to_id
)
DELETE FROM edges WHERE id IN (SELECT id FROM ph) AND id NOT IN (SELECT id FROM keep)`)
	return err
}

// schemaPlan is the decision planSchemaMigration derives from the stored
// PRAGMA user_version. It mutates nothing on its own.
type schemaPlan struct {
	wipe    bool              // drop the on-disk DB and rebuild from source
	inPlace []schemaMigration // ordered in-place steps to run after schemaSQL
	stamp   bool              // write currentSchemaVersion once reconciled
}

// planSchemaMigrationWith decides how to reconcile a store at the stored
// PRAGMA user_version to current, given the migration registry. It mutates
// nothing. Open passes (currentSchemaVersion, schemaMigrations); tests pass
// fixtures.
func planSchemaMigrationWith(stored, current int, migrations []schemaMigration) schemaPlan {
	switch {
	case stored == current:
		return schemaPlan{} // up to date, nothing to do
	case stored > current:
		// Written by a newer build than this binary understands; the shape may
		// have changed under us. For a cache the safe move is to rebuild.
		return schemaPlan{wipe: true, stamp: true}
	case stored == 0:
		// Fresh DB, or a pre-versioning store of unknown shape. schemaSQL's
		// idempotent CREATE ... IF NOT EXISTS plus ensureNodeColumns /
		// ensureEdgeColumns reconcile the base shape either way, so a stored==0
		// store needs a wipe only when a pending step is a REBUILD whose data can
		// only come from re-indexing source. With nothing pending, stamp; with
		// only in-place steps pending, run them and stamp — an in-place step is
		// idempotent and mechanically derivable, so it upgrades a pre-versioning
		// store in place (preserving its rows) exactly as it upgrades a known
		// prior version. Wiping a stored==0 store on any migration instead would
		// force every non-daemon Open (tests, read-only tools) to pass WithRebuild
		// the moment the first migration ships.
		pending := pendingBetween(0, current, migrations)
		if len(pending) == 0 {
			return schemaPlan{stamp: true}
		}
		if anyRebuild(pending) {
			return schemaPlan{wipe: true, stamp: true}
		}
		return schemaPlan{inPlace: pending, stamp: true}
	default: // 0 < stored < current: a known prior version
		pending := pendingBetween(stored, current, migrations)
		if anyRebuild(pending) {
			return schemaPlan{wipe: true, stamp: true}
		}
		return schemaPlan{inPlace: pending, stamp: true}
	}
}

func pendingBetween(stored, current int, migrations []schemaMigration) []schemaMigration {
	var out []schemaMigration
	for _, m := range migrations {
		if m.version > stored && m.version <= current {
			out = append(out, m)
		}
	}
	return out
}

func anyRebuild(ms []schemaMigration) bool {
	for _, m := range ms {
		if m.rebuild {
			return true
		}
	}
	return false
}

// validateSchemaMigrations checks the registry is well-formed. A test asserts
// this against the shipped (currentSchemaVersion, schemaMigrations) so the
// dangerous mistake — bumping currentSchemaVersion without appending a matching
// entry — fails CI instead of silently baseline-stamping an un-migrated store
// to the new version at runtime. Rules:
//   - versions are >= 2 (v1 is the implicit baseline, never an entry) and
//     strictly ascending;
//   - each step sets exactly one strategy (inPlace xor rebuild);
//   - the highest version equals current, so the registry actually defines how
//     to reach it. An empty registry is valid only at version 1.
func validateSchemaMigrations(current int, migs []schemaMigration) error {
	if len(migs) == 0 {
		if current != 1 {
			return fmt.Errorf("schema version %d has no migrations: only v1 may have an empty registry", current)
		}
		return nil
	}
	prev := 0
	for i, m := range migs {
		if m.version < 2 {
			return fmt.Errorf("migration %q has version %d: entries must be >= 2 (v1 is the implicit baseline)", m.name, m.version)
		}
		if i > 0 && m.version <= prev {
			return fmt.Errorf("migrations must be strictly ascending: v%d (%s) does not follow v%d", m.version, m.name, prev)
		}
		if (m.inPlace != nil) == m.rebuild {
			return fmt.Errorf("migration v%d (%s) must set exactly one of inPlace / rebuild", m.version, m.name)
		}
		prev = m.version
	}
	if prev != current {
		return fmt.Errorf("highest migration version %d != currentSchemaVersion %d: a version bump needs a matching migration entry", prev, current)
	}
	return nil
}

// readUserVersion reads PRAGMA user_version (0 on a fresh database).
func readUserVersion(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}

// setUserVersion stamps the schema version. PRAGMA takes no bound parameters;
// v is an int we control, so the format is safe.
func setUserVersion(db *sql.DB, v int) error {
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", v)); err != nil {
		return err
	}
	return nil
}

// applyInPlaceMigrations runs the in-place steps in a single transaction.
func applyInPlaceMigrations(db *sql.DB, steps []schemaMigration) error {
	if len(steps) == 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }() // no-op once Commit succeeds
	for _, m := range steps {
		if err := m.inPlace(tx); err != nil {
			return fmt.Errorf("schema migration v%d (%s): %w", m.version, m.name, err)
		}
	}
	return tx.Commit()
}

// removeStoreFiles deletes the SQLite database and its companions. A missing
// file is not an error. Never called for ":memory:".
//
// The suffix list covers the files the DSN's journal_mode(WAL) produces (-wal,
// -shm) plus the rollback -journal a non-WAL fallback would use; keep it in
// sync if the journal_mode in Open's DSN ever changes.
func removeStoreFiles(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path+suffix, err)
		}
	}
	return nil
}

// isMemoryPath reports whether path is an in-process SQLite database (no file
// on disk to wipe, always built fresh by schemaSQL).
func isMemoryPath(path string) bool {
	return strings.Contains(path, ":memory:")
}
