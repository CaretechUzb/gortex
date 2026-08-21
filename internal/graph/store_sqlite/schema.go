package store_sqlite

import "database/sql"

// isUnresolvedColumnDDL is the edges.is_unresolved generated column: a
// VIRTUAL, indexed boolean mirroring graph.IsUnresolvedTarget's two shapes
// (the bare `unresolved::Name` prefix and the multi-repo COPY-rewrite
// `<repoPrefix>::unresolved::Name` infix), computed by SQLite itself from
// to_id — no Go call site has to remember to keep it in sync. VIRTUAL, not
// STORED: SQLite refuses `ALTER TABLE ADD COLUMN ... STORED` on a non-empty
// table ("cannot add a STORED column"), which every real installed store is.
// VIRTUAL has no such restriction and is just as fast here — the read path
// always goes through the index below, and an index always stores its own
// materialised key values regardless of whether the underlying column is
// virtual or stored. Added via ensureEdgeColumns (ALTER TABLE) rather than
// baked into schemaSQL's CREATE TABLE so one code path handles both a fresh
// DB (column missing right after CREATE TABLE) and an existing one (column
// missing from before this was introduced) identically — see
// ensureNodeColumns for the same pattern on the nodes table.
//
// Measured on a real 26-repo store (2.57M edges, 847,684 unresolved, ~33%
// selectivity): replacing the OR'd `to_id` range/LIKE query with
// `is_unresolved = 1` cut EdgesWithUnresolvedTarget from 7.96s to 2.95s
// (2.7x). The prior approach of splitting the OR into two to_id-based
// queries (one indexed range, one LIKE) was WORSE (13.49s) despite a
// better-looking EXPLAIN QUERY PLAN: at ~33% selectivity the to_id index's
// matching rows are ordered by string value, so the mandatory per-row
// bookmark lookup back into the main table is effectively random I/O. The
// boolean column's matching rows are all rowid-tie-broken (identical index
// key), so its bookmark lookups land in ascending rowid order — sequential,
// not random. Same "SEARCH ... USING INDEX" in EXPLAIN QUERY PLAN either way;
// only real measurement told them apart.
const isUnresolvedColumnDDL = `is_unresolved INTEGER GENERATED ALWAYS AS (
    CASE WHEN (to_id >= 'unresolved::' AND to_id < 'unresolved:;') OR to_id LIKE '%::unresolved::%' THEN 1 ELSE 0 END
) VIRTUAL`

// memberReceiver* are virtual projections of the final `<file>::<type>`
// segment carried by a Go member_of edge target. rtrim(X, replace(X, ':',
// ”)) stops at X's final colon, which gives the final `::` delimiter without
// a reverse() user function. All graph IDs use colons only as `::`
// separators, so the two substrings exactly mirror strings.LastIndex(to,
// "::") in the resolver fallback while remaining indexable by SQLite.
const (
	memberReceiverPrefixExpr = `rtrim(to_id, replace(to_id, ':', ''))`
	memberReceiverFileExpr   = `substr(to_id, 1, length(` + memberReceiverPrefixExpr + `) - 2)`
	memberReceiverNameExpr   = `substr(to_id, length(` + memberReceiverPrefixExpr + `) + 1)`

	memberReceiverColumnDDL = `member_receiver TEXT GENERATED ALWAYS AS (
    CASE WHEN kind = 'member_of' AND instr(to_id, '::') > 1
              AND ` + memberReceiverNameExpr + ` <> ''
         THEN ` + memberReceiverNameExpr + ` ELSE NULL END
) VIRTUAL`

	// filepath.Dir for normalized graph paths, expressed with built-in SQLite
	// string functions so it can back an index on existing databases without
	// materializing or backfilling another copy of the value. The file
	// segment is separator-normalized like fileDirColumnDDL so the rebind
	// join's two sides agree on native-separator stores; changing the
	// expression needs a schemaMigrations entry, like file_dir.
	memberReceiverNormFileExpr = `replace(` + memberReceiverFileExpr + `, '\', '/')`

	memberReceiverDirColumnDDL = `member_receiver_dir TEXT GENERATED ALWAYS AS (
    CASE WHEN kind <> 'member_of' OR instr(to_id, '::') <= 1
              OR ` + memberReceiverNameExpr + ` = '' THEN NULL
         WHEN instr(` + memberReceiverNormFileExpr + `, '/') = 0 THEN '.'
         WHEN rtrim(rtrim(` + memberReceiverNormFileExpr + `,
                         replace(` + memberReceiverNormFileExpr + `, '/', '')), '/') = '' THEN '/'
         ELSE rtrim(rtrim(` + memberReceiverNormFileExpr + `,
                         replace(` + memberReceiverNormFileExpr + `, '/', '')), '/') END
) VIRTUAL`
)

// edgeGeneratedColumns is the set of edges.* generated columns ensureEdgeColumns
// adds to a table created before they existed — which, since none of them are
// in schemaSQL's CREATE TABLE, includes a freshly created table too.
// fromRepoColumnDDL mirrors graph.RepoPrefixOfID exactly: the substring
// before the first '/' when that slash is past position 1, else ” (bare
// unresolved sentinels and single-repo-mode ids). Same rationale as
// isUnresolvedColumnDDL: computed by SQLite from from_id, no Go call site
// can forget to keep it in sync, and the scoped resolver pushdown can test
// repo membership as a plain column. Parity is asserted against the Go
// helper by TestEdgeScopeColumnsMirrorGoHelpers.
const fromRepoColumnDDL = `from_repo TEXT GENERATED ALWAYS AS (
    CASE WHEN instr(from_id, '/') > 1 THEN substr(from_id, 1, instr(from_id, '/') - 1) ELSE '' END
) VIRTUAL`

// toRepoUnresolvedColumnDDL mirrors graph.UnresolvedRepoPrefix exactly for
// the shapes the pending scan can see (is_unresolved = 1 guarantees one of
// the two unresolved encodings): ” for the bare `unresolved::` prefix form,
// the leading `<repo>` for the `<repo>::unresolved::` infix form, NULL for
// anything else. NULL is deliberately fail-open — a consumer predicate must
// treat it as "load the row and let the Go filter decide".
const toRepoUnresolvedColumnDDL = `to_repo_unresolved TEXT GENERATED ALWAYS AS (
    CASE WHEN to_id >= 'unresolved::' AND to_id < 'unresolved:;' THEN ''
         WHEN instr(to_id, '::unresolved::') > 1 THEN substr(to_id, 1, instr(to_id, '::unresolved::') - 1)
         ELSE NULL END
) VIRTUAL`

var edgeGeneratedColumns = []struct {
	name string
	ddl  string
}{
	{"is_unresolved", isUnresolvedColumnDDL},
	{"member_receiver", memberReceiverColumnDDL},
	{"member_receiver_dir", memberReceiverDirColumnDDL},
	{"from_repo", fromRepoColumnDDL},
	{"to_repo_unresolved", toRepoUnresolvedColumnDDL},
}

// edgePromotedColumns lifts the resolver's resolve_terminal /
// resolve_terminal_reason keys and semantic_source provenance out of Meta
// into nullable columns — the edge-side sibling of promotedMetaColumns on
// nodes (see meta_json.go's "promoted edge columns" section for
// extractPromotedEdgeMeta/restorePromotedEdgeMeta and why a
// json_extract-derived generated column was tried first and abandoned:
// encodeMeta's common case is a custom flat binary codec, not JSON, so
// json_extract/json_valid against a real store's meta blobs evaluates to
// NULL for effectively every row). Plain (non-generated) columns, so they
// share ensureEdgeColumns' table_xinfo scan but are ordinary ALTER TABLE ADD
// COLUMN statements, not GENERATED ALWAYS AS expressions.
//
// Exists to let a future bulk classification query (replacing per-edge
// Go-side classifyTerminal calls in reconcileTerminalStamps) read the
// CURRENT terminal state as a plain indexed column instead of decoding
// Meta, and compare it against a freshly computed value to find only the
// edges whose state actually changed — reconcileTerminalStamps measured
// only ~1% of examined edges (9,599 of 833,828) ever change state.
var edgePromotedColumns = []struct {
	name string
	ddl  string
}{
	{"resolve_terminal", "resolve_terminal INTEGER"},
	{"resolve_terminal_reason", "resolve_terminal_reason TEXT"},
	{"semantic_source", "semantic_source TEXT"},
}

// ensureEdgeColumns adds edgeGeneratedColumns + edgePromotedColumns to an
// edges table created before they existed. Mirrors ensureNodeColumns'
// PRAGMA + conditional ALTER pattern, but queries table_xinfo rather than
// table_info: table_info silently OMITS generated columns from its result
// set (verified against the pinned modernc.org/sqlite driver — a reopened
// store's is_unresolved column is invisible to table_info, so the existence
// check always came back false and every reopen re-ran the ALTER, failing
// with "duplicate column name"). table_xinfo lists every column, generated
// ones included, with an extra hidden column (3 == generated) table_info
// doesn't have — and works identically for the plain promoted columns too,
// so one scan serves both lists.
func ensureEdgeColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_xinfo(edges)`)
	if err != nil {
		return err
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notnull, pk, hidden int
			name, ctype              string
			dflt                     sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk, &hidden); err != nil {
			_ = rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, c := range edgeGeneratedColumns {
		if existing[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE edges ADD COLUMN ` + c.ddl); err != nil {
			return err
		}
	}
	for _, c := range edgePromotedColumns {
		if existing[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE edges ADD COLUMN ` + c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// isStubColumnDDL is nodes.is_stub: a VIRTUAL generated column mirroring
// graph.IsStub/StubKind's id-prefix logic (stdlib:: / builtin:: /
// external_call:: / module::, bare or repo-prefixed as <repo>::<kind>::...).
// Same rationale as isUnresolvedColumnDDL: computed from the existing id
// column, no Go call site has to keep it in sync. Exists so a future
// SQL-side terminal classification (see resolveTerminalColumnDDL) can check
// "is this candidate a stub" via a plain column instead of a per-row Go
// IsStub(n.ID) call.
const isStubColumnDDL = `is_stub INTEGER GENERATED ALWAYS AS (
    CASE WHEN
        id LIKE 'stdlib::%' OR id LIKE 'builtin::%' OR id LIKE 'external_call::%' OR id LIKE 'module::%'
        OR (
            instr(id, '::') > 0 AND (
                substr(id, instr(id, '::') + 2) LIKE 'stdlib::%'
                OR substr(id, instr(id, '::') + 2) LIKE 'builtin::%'
                OR substr(id, instr(id, '::') + 2) LIKE 'external_call::%'
                OR substr(id, instr(id, '::') + 2) LIKE 'module::%'
            )
        )
    THEN 1 ELSE 0 END
) VIRTUAL`

// fileDirColumnDDL promotes filepath.Dir(file_path) into an indexable virtual
// column. It adds no row payload and is migration-safe on populated stores;
// the receiver-rebind join uses it to seek directly to Go types/interfaces in
// the phantom receiver's package instead of loading every type into memory.
// Stored paths keep the writing platform's native separators below the repo
// prefix, so the expression normalizes '\' to '/' before trimming — the SQL
// mirror of graphpath.Norm + Dir. Without it a Windows-written store
// collapses every file_dir to the repo prefix and the rebind join
// degenerates from "same package dir" to "same repo". Changing this
// expression needs a schemaMigrations entry: a generated column on an
// existing store is rebuilt, never altered in place.
const fileDirSourceExpr = `replace(file_path, '\', '/')`

const fileDirColumnDDL = `file_dir TEXT GENERATED ALWAYS AS (
    CASE WHEN file_path = '' THEN NULL
         WHEN instr(` + fileDirSourceExpr + `, '/') = 0 THEN '.'
         WHEN rtrim(rtrim(` + fileDirSourceExpr + `, replace(` + fileDirSourceExpr + `, '/', '')), '/') = '' THEN '/'
         ELSE rtrim(rtrim(` + fileDirSourceExpr + `, replace(` + fileDirSourceExpr + `, '/', '')), '/') END
) VIRTUAL`

// nodeGeneratedColumns is the nodes-table sibling of edgeGeneratedColumns.
// Kept as its own list (and ensureNodeGeneratedColumns as its own function,
// rather than folded into ensureNodeColumns) because ensureNodeColumns
// checks existence via PRAGMA table_info, which — like the edges case
// documented on ensureEdgeColumns — silently omits generated columns.
// Reusing that function's table_info scan for is_stub would hit the exact
// same "always looks missing, ALTER re-runs, duplicate column name" bug.
var nodeGeneratedColumns = []struct {
	name string
	ddl  string
}{
	{"is_stub", isStubColumnDDL},
	{"file_dir", fileDirColumnDDL},
}

// ensureNodeGeneratedColumns adds nodeGeneratedColumns to a nodes table
// created before they existed. See ensureEdgeColumns for the table_xinfo
// vs table_info rationale this mirrors.
func ensureNodeGeneratedColumns(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_xinfo(nodes)`)
	if err != nil {
		return err
	}
	existing := make(map[string]bool)
	for rows.Next() {
		var (
			cid, notnull, pk, hidden int
			name, ctype              string
			dflt                     sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk, &hidden); err != nil {
			_ = rows.Close()
			return err
		}
		existing[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, c := range nodeGeneratedColumns {
		if existing[c.name] {
			continue
		}
		if _, err := db.Exec(`ALTER TABLE nodes ADD COLUMN ` + c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// analysisGenerationSchemaSQL is the normalized, generation-addressed
// whole-graph analysis cache. Node IDs are copied into generation-local rows:
// they deliberately do not reference live nodes because incremental reindex
// deletes and recreates live rows, which would either erase an immutable
// snapshot or turn a tiny edit into a large FK cascade. Dense CSR and Leiden
// state remain blobs; point-queryable results use typed rows and compact
// surrogate node IDs.
const analysisGenerationSchemaSQL = `
CREATE TABLE IF NOT EXISTS analysis_generations (
    generation_id               INTEGER PRIMARY KEY AUTOINCREMENT,
    format_version              INTEGER NOT NULL,
    build_revision              INTEGER NOT NULL,
    created_at_unix             INTEGER NOT NULL,
    state                       INTEGER NOT NULL CHECK (state IN (0, 1, 2)),
    node_count                  INTEGER NOT NULL CHECK (node_count >= 0),
    community_count             INTEGER NOT NULL CHECK (community_count >= 0),
    process_count               INTEGER NOT NULL CHECK (process_count >= 0),
    concept_count               INTEGER NOT NULL CHECK (concept_count >= 0),
    pagerank_max                REAL NOT NULL DEFAULT 0,
    authority_max               REAL NOT NULL DEFAULT 0,
    hub_max                     REAL NOT NULL DEFAULT 0,
    modularity                  REAL NOT NULL DEFAULT 0,
    processes_truncated         INTEGER NOT NULL DEFAULT 0 CHECK (processes_truncated IN (0, 1)),
    processes_truncation_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS analysis_generations_by_state
    ON analysis_generations(state, generation_id DESC);

CREATE TABLE IF NOT EXISTS analysis_active_generation (
    slot          INTEGER PRIMARY KEY CHECK (slot = 1),
    generation_id INTEGER NOT NULL UNIQUE
        REFERENCES analysis_generations(generation_id) ON DELETE RESTRICT
);

CREATE TABLE IF NOT EXISTS analysis_generation_components (
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    component     TEXT NOT NULL,
    row_count     INTEGER NOT NULL CHECK (row_count >= 0),
    sealed_at_unix INTEGER NOT NULL,
    PRIMARY KEY (generation_id, component)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS analysis_communities (
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    community_id  TEXT NOT NULL,
    label         TEXT NOT NULL DEFAULT '',
    hub            TEXT NOT NULL DEFAULT '',
    parent_id      TEXT NOT NULL DEFAULT '',
    size           INTEGER NOT NULL CHECK (size >= 0),
    cohesion       REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (generation_id, community_id)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS analysis_community_files (
    generation_id INTEGER NOT NULL,
    community_id  TEXT NOT NULL,
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    file_path     TEXT NOT NULL,
    PRIMARY KEY (generation_id, community_id, ordinal),
    FOREIGN KEY (generation_id, community_id)
        REFERENCES analysis_communities(generation_id, community_id) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS analysis_nodes (
    id            INTEGER PRIMARY KEY,
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    node_id       TEXT NOT NULL,
    community_id  TEXT,
    pagerank      REAL NOT NULL DEFAULT 0,
    authority     REAL NOT NULL DEFAULT 0,
    hub           REAL NOT NULL DEFAULT 0,
    UNIQUE (generation_id, node_id),
    FOREIGN KEY (generation_id, community_id)
        REFERENCES analysis_communities(generation_id, community_id)
);
CREATE INDEX IF NOT EXISTS analysis_nodes_by_pagerank
    ON analysis_nodes(generation_id, pagerank DESC, id ASC);
CREATE INDEX IF NOT EXISTS analysis_nodes_by_authority
    ON analysis_nodes(generation_id, authority DESC, id ASC);
CREATE INDEX IF NOT EXISTS analysis_nodes_by_hub
    ON analysis_nodes(generation_id, hub DESC, id ASC);
CREATE INDEX IF NOT EXISTS analysis_nodes_by_community
    ON analysis_nodes(generation_id, community_id, node_id);

CREATE TABLE IF NOT EXISTS analysis_processes (
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    process_id     TEXT NOT NULL,
    name           TEXT NOT NULL DEFAULT '',
    entry_point    TEXT NOT NULL DEFAULT '',
    step_count     INTEGER NOT NULL CHECK (step_count >= 0),
    score          REAL NOT NULL DEFAULT 0,
    truncated      INTEGER NOT NULL DEFAULT 0 CHECK (truncated IN (0, 1)),
    PRIMARY KEY (generation_id, process_id)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS analysis_process_files (
    generation_id INTEGER NOT NULL,
    process_id    TEXT NOT NULL,
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    file_path     TEXT NOT NULL,
    PRIMARY KEY (generation_id, process_id, ordinal),
    FOREIGN KEY (generation_id, process_id)
        REFERENCES analysis_processes(generation_id, process_id) ON DELETE CASCADE
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS analysis_process_steps (
    generation_id INTEGER NOT NULL,
    process_id    TEXT NOT NULL,
    ordinal       INTEGER NOT NULL CHECK (ordinal >= 0),
    node_rowid    INTEGER NOT NULL
        REFERENCES analysis_nodes(id),
    depth         INTEGER NOT NULL CHECK (depth >= 0),
    PRIMARY KEY (generation_id, process_id, ordinal),
    FOREIGN KEY (generation_id, process_id)
        REFERENCES analysis_processes(generation_id, process_id) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS analysis_process_steps_by_node
    ON analysis_process_steps(generation_id, node_rowid, process_id);

CREATE TABLE IF NOT EXISTS analysis_concepts (
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    token         TEXT NOT NULL,
    in_vocabulary INTEGER NOT NULL CHECK (in_vocabulary IN (0, 1)),
    PRIMARY KEY (generation_id, token)
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS analysis_concept_relations (
    generation_id INTEGER NOT NULL,
    token         TEXT NOT NULL,
    related_token TEXT NOT NULL,
    rank          INTEGER NOT NULL CHECK (rank >= 0),
    PRIMARY KEY (generation_id, token, rank, related_token),
    FOREIGN KEY (generation_id, token)
        REFERENCES analysis_concepts(generation_id, token) ON DELETE CASCADE,
    FOREIGN KEY (generation_id, related_token)
        REFERENCES analysis_concepts(generation_id, token) ON DELETE CASCADE
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS analysis_concept_relations_by_related
    ON analysis_concept_relations(generation_id, related_token, rank, token);

CREATE TABLE IF NOT EXISTS analysis_blobs (
    generation_id INTEGER NOT NULL
        REFERENCES analysis_generations(generation_id) ON DELETE CASCADE,
    component     TEXT NOT NULL,
    payload       BLOB NOT NULL,
    PRIMARY KEY (generation_id, component)
) WITHOUT ROWID;
`

// checkoutCatalogSchemaSQL is the checkout-lifecycle control plane: which
// working copies the daemon knows about, what it intends to do with each one,
// which graph and view generation a checkout currently routes to, and what is
// queued for cleanup. It is a CONTROL plane, not a payload one — no table here
// references nodes / edges / files / vectors, and nothing here re-keys them.
// Payload rows are replaced wholesale by an incremental reindex, so a foreign
// key into them would either erase control-plane state or turn a one-file edit
// into a large cascade; the same reasoning keeps analysis generations detached.
//
// Foreign keys inside the catalog are load-bearing and rely on the store's
// per-connection foreign_keys(ON) pragma (see sqlitePerConnectionPragmasBase):
//
//   - ON DELETE CASCADE ties a checkout's intents, its in-flight intent
//     transition and its path evidence to the checkout row, so forgetting a
//     checkout cannot strand them.
//   - ON DELETE RESTRICT on every generation pointer (checkout_routes,
//     ref_views, view_generations.base_generation_id) and on the ownership
//     edges (a checkout's family, a dedicated graph's family and owner, a
//     route's checkout) refuses the delete instead of silently rewriting the
//     child. SQLite's own default — NO ACTION on a non-deferred constraint —
//     already refuses it; naming RESTRICT makes the intent readable and
//     matches analysis_active_generation. Go's guarded delete helper reports
//     the same refusal as a typed error before SQLite has to.
//   - cleanup_journal deliberately carries NO foreign keys. It records work
//     that outlives the rows it is cleaning up, so the row whose deletion the
//     journal entry describes must be free to disappear first.
//
// State columns are plain TEXT; the Go layer owns their vocabularies (see
// catalog_types.go) because a CHECK constraint would freeze the enum into
// every installed database file and force a migration to extend it.
//
// Timestamp columns are unix seconds supplied by the caller, like
// repo_index_state.indexed_at — no helper here reads the clock, so a guarded
// transition is reproducible in a test.
const checkoutCatalogSchemaSQL = `
CREATE TABLE IF NOT EXISTS repository_families (
    family_id           TEXT PRIMARY KEY,
    common_dir_identity TEXT NOT NULL UNIQUE,
    display_remote      TEXT NOT NULL DEFAULT '',
    state               TEXT NOT NULL,
    primary_epoch       INTEGER NOT NULL DEFAULT 0,
    created_at          INTEGER NOT NULL DEFAULT 0,
    last_seen           INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

-- One row per working copy (a primary checkout or a linked worktree).
-- incarnation distinguishes a path that was removed and recreated from the
-- one the daemon has been tracking, so every guarded write carries it and a
-- write aimed at the previous incarnation changes nothing. The UNIQUE key
-- also serves family-scoped listing, so no separate by-family index exists.
CREATE TABLE IF NOT EXISTS checkouts (
    checkout_id                 TEXT PRIMARY KEY,
    incarnation                 TEXT NOT NULL,
    family_id                   TEXT NOT NULL
        REFERENCES repository_families(family_id) ON DELETE RESTRICT,
    root_path                   TEXT NOT NULL,
    git_dir                     TEXT NOT NULL,
    admin_name                  TEXT NOT NULL,
    state                       TEXT NOT NULL,
    desired_mode                TEXT NOT NULL,
    effective_mode              TEXT NOT NULL,
    locked                      INTEGER NOT NULL DEFAULT 0,
    prunable                    INTEGER NOT NULL DEFAULT 0,
    head_ref                    TEXT NOT NULL DEFAULT '',
    head_commit                 TEXT NOT NULL DEFAULT '',
    head_tree                   TEXT NOT NULL DEFAULT '',
    last_accessible             INTEGER NOT NULL DEFAULT 0,
    unavailable_since           INTEGER NOT NULL DEFAULT 0,
    availability_deadline       INTEGER NOT NULL DEFAULT 0,
    removal_detected_at         INTEGER NOT NULL DEFAULT 0,
    removal_deadline            INTEGER NOT NULL DEFAULT 0,
    removal_evidence            TEXT NOT NULL DEFAULT '',
    active_intent_transition_id TEXT,
    last_seen                   INTEGER NOT NULL DEFAULT 0,
    last_error                  TEXT NOT NULL DEFAULT '',
    UNIQUE (family_id, admin_name, incarnation)
) WITHOUT ROWID;

-- Why the daemon tracks a checkout at all. Several sources may independently
-- ask for the same checkout; the UNIQUE key makes a repeated request from one
-- source an update instead of a duplicate, and revoking one source leaves the
-- others intact. It also backs the per-checkout listing.
CREATE TABLE IF NOT EXISTS tracking_intents (
    intent_id      TEXT PRIMARY KEY,
    checkout_id    TEXT NOT NULL REFERENCES checkouts(checkout_id) ON DELETE CASCADE,
    source_kind    TEXT NOT NULL,
    source_locator TEXT NOT NULL,
    active         INTEGER NOT NULL,
    created_at     INTEGER NOT NULL DEFAULT 0,
    revoked_at     INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT NOT NULL DEFAULT '',
    UNIQUE (checkout_id, source_kind, source_locator)
) WITHOUT ROWID;

-- The single in-flight mode change for a checkout. UNIQUE(checkout_id) is the
-- concurrency control: a second transition cannot begin while one is
-- outstanding, and the prior_* columns capture what to restore if it fails.
CREATE TABLE IF NOT EXISTS intent_transitions (
    transition_id        TEXT PRIMARY KEY,
    checkout_id          TEXT NOT NULL UNIQUE
        REFERENCES checkouts(checkout_id) ON DELETE CASCADE,
    cause                TEXT NOT NULL,
    prior_desired_mode   TEXT,
    prior_effective_mode TEXT,
    requested_mode       TEXT,
    prior_checkout_state TEXT,
    source_snapshot_hash TEXT,
    state                TEXT NOT NULL,
    created_at           INTEGER NOT NULL DEFAULT 0,
    last_progress        INTEGER NOT NULL DEFAULT 0,
    last_error           TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- What the filesystem looked like the last time a checkout's path was
-- sampled. It answers "is this path gone, or is its volume merely detached"
-- without re-walking the filesystem, so an unmounted disk does not read as a
-- deletion. One row per checkout, replaced by each sample.
CREATE TABLE IF NOT EXISTS checkout_path_evidence (
    checkout_id                    TEXT PRIMARY KEY
        REFERENCES checkouts(checkout_id) ON DELETE CASCADE,
    root_path_identity             TEXT NOT NULL DEFAULT '',
    root_volume_kind               TEXT NOT NULL DEFAULT '',
    root_volume_token              TEXT NOT NULL DEFAULT '',
    nearest_existing_ancestor_path TEXT NOT NULL DEFAULT '',
    ancestor_volume_kind           TEXT NOT NULL DEFAULT '',
    ancestor_volume_token          TEXT NOT NULL DEFAULT '',
    common_dir_volume_kind         TEXT NOT NULL DEFAULT '',
    common_dir_volume_token        TEXT NOT NULL DEFAULT '',
    sampled_at                     INTEGER NOT NULL DEFAULT 0,
    sample_generation              INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

-- A graph built for one checkout. repo_prefix is UNIQUE because it is the
-- namespace its nodes carry, and owner_checkout_id is UNIQUE because a
-- checkout owns at most one dedicated graph. active_generation_id is a plain
-- integer rather than a foreign key: a dedicated graph outlives the
-- generations it publishes, and the guarded delete helper checks this pointer
-- in Go so pruning a generation cannot silently strand a graph.
CREATE TABLE IF NOT EXISTS dedicated_graphs (
    graph_id             TEXT PRIMARY KEY,
    owner_checkout_id    TEXT UNIQUE
        REFERENCES checkouts(checkout_id) ON DELETE RESTRICT,
    repo_prefix          TEXT NOT NULL UNIQUE,
    family_id            TEXT NOT NULL
        REFERENCES repository_families(family_id) ON DELETE RESTRICT,
    is_primary_base      INTEGER NOT NULL DEFAULT 0,
    active_generation_id INTEGER,
    state                TEXT NOT NULL
) WITHOUT ROWID;
-- At most one primary base per family. Partial, so the many non-primary rows
-- of a family do not collide with each other.
CREATE UNIQUE INDEX IF NOT EXISTS dedicated_graphs_primary_base
    ON dedicated_graphs(family_id) WHERE is_primary_base = 1;

-- An immutable-once-ready build of a view. Named view_generations rather than
-- "generations" so it cannot be confused with the analysis_generations cache,
-- which is a different lifecycle over the same word. base_generation_id is
-- self-referential: an overlay generation names the committed generation it
-- was layered onto, and that parent cannot be deleted while it does.
CREATE TABLE IF NOT EXISTS view_generations (
    generation_id          INTEGER PRIMARY KEY AUTOINCREMENT,
    owner_kind             TEXT NOT NULL,
    graph_id               TEXT NOT NULL DEFAULT '',
    layer_id               TEXT,
    checkout_id            TEXT,
    generation_kind        TEXT NOT NULL,
    base_generation_id     INTEGER
        REFERENCES view_generations(generation_id) ON DELETE RESTRICT,
    lower_view_fingerprint TEXT NOT NULL DEFAULT '',
    tree_oid               TEXT NOT NULL DEFAULT '',
    provenance_commit_oid  TEXT,
    config_hash            TEXT NOT NULL DEFAULT '',
    extractor_versions     TEXT NOT NULL DEFAULT '',
    resolver_version       TEXT NOT NULL DEFAULT '',
    state                  TEXT NOT NULL,
    covered_files          INTEGER NOT NULL DEFAULT 0,
    affected_files         INTEGER NOT NULL DEFAULT 0,
    storage_bytes          INTEGER NOT NULL DEFAULT 0,
    completeness           TEXT NOT NULL DEFAULT '',
    created_at             INTEGER NOT NULL DEFAULT 0,
    published_at           INTEGER NOT NULL DEFAULT 0,
    last_selected          INTEGER NOT NULL DEFAULT 0,
    error                  TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS view_generations_by_graph_state
    ON view_generations(graph_id, state, generation_id DESC);
-- Indexes the child side of the self-reference so the delete guard, and
-- SQLite's own foreign-key check, probe instead of scanning the table.
CREATE INDEX IF NOT EXISTS view_generations_by_base
    ON view_generations(base_generation_id) WHERE base_generation_id IS NOT NULL;

CREATE TABLE IF NOT EXISTS view_layers (
    layer_id      TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    graph_id      TEXT NOT NULL,
    checkout_id   TEXT,
    target_ref    TEXT,
    target_commit TEXT NOT NULL DEFAULT '',
    target_tree   TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- Where a checkout's queries land right now. route_epoch is the compare-and-
-- set token for a route flip: a flip that carries a stale epoch changes
-- nothing, so two reconcilers cannot interleave halves of two different
-- routes. The two partial indexes keep the generation delete guard (and
-- SQLite's foreign-key check) off a table scan.
CREATE TABLE IF NOT EXISTS checkout_routes (
    checkout_id          TEXT PRIMARY KEY
        REFERENCES checkouts(checkout_id) ON DELETE RESTRICT,
    graph_id             TEXT NOT NULL,
    commit_generation_id INTEGER
        REFERENCES view_generations(generation_id) ON DELETE RESTRICT,
    dirty_generation_id  INTEGER
        REFERENCES view_generations(generation_id) ON DELETE RESTRICT,
    route_epoch          INTEGER NOT NULL DEFAULT 0,
    state                TEXT NOT NULL
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS checkout_routes_by_commit_generation
    ON checkout_routes(commit_generation_id) WHERE commit_generation_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS checkout_routes_by_dirty_generation
    ON checkout_routes(dirty_generation_id) WHERE dirty_generation_id IS NOT NULL;

-- A named view of one graph at a selector (a branch, a tag, a commit). The
-- desired_* columns are what was asked for; the active_* columns are what is
-- actually serving, and they diverge while a build is in flight.
CREATE TABLE IF NOT EXISTS ref_views (
    ref_view_id               TEXT PRIMARY KEY,
    graph_id                  TEXT NOT NULL,
    selector_kind             TEXT NOT NULL,
    selector_value            TEXT NOT NULL,
    desired_ref               TEXT NOT NULL DEFAULT '',
    desired_commit            TEXT NOT NULL DEFAULT '',
    desired_tree              TEXT NOT NULL DEFAULT '',
    active_generation_id      INTEGER
        REFERENCES view_generations(generation_id) ON DELETE RESTRICT,
    active_ref                TEXT,
    active_commit             TEXT,
    active_tree               TEXT,
    enrichment_profile        TEXT NOT NULL,
    desired_build_fingerprint TEXT NOT NULL DEFAULT '',
    active_build_fingerprint  TEXT,
    route_epoch               INTEGER NOT NULL DEFAULT 0,
    state                     TEXT NOT NULL,
    exact_view                INTEGER NOT NULL DEFAULT 1,
    last_resolved             INTEGER NOT NULL DEFAULT 0,
    last_selected             INTEGER NOT NULL DEFAULT 0,
    last_error                TEXT NOT NULL DEFAULT '',
    UNIQUE (graph_id, selector_kind, selector_value, enrichment_profile)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS ref_views_by_active_generation
    ON ref_views(active_generation_id) WHERE active_generation_id IS NOT NULL;

-- One build attempt for a ref view. build_token is UNIQUE so a worker can
-- prove it still owns the attempt it started.
CREATE TABLE IF NOT EXISTS ref_view_builds (
    build_id             TEXT PRIMARY KEY,
    ref_view_id          TEXT NOT NULL REFERENCES ref_views(ref_view_id) ON DELETE CASCADE,
    desired_ref          TEXT NOT NULL DEFAULT '',
    desired_commit       TEXT NOT NULL DEFAULT '',
    desired_tree         TEXT NOT NULL DEFAULT '',
    base_generation_id   INTEGER,
    enrichment_profile   TEXT NOT NULL DEFAULT '',
    build_fingerprint    TEXT NOT NULL,
    generation_id        INTEGER,
    captured_route_epoch INTEGER NOT NULL,
    state                TEXT NOT NULL,
    build_token          TEXT NOT NULL UNIQUE,
    created_at           INTEGER NOT NULL DEFAULT 0,
    last_progress        INTEGER NOT NULL DEFAULT 0,
    error                TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
-- Coalescing: two requests for the same tree, base and fingerprint share one
-- in-flight build instead of racing to produce the same generation twice.
-- Only 'building' rows participate, so finished attempts accumulate freely.
-- SQLite treats NULLs as distinct in a unique index, so a build with no base
-- generation is deliberately outside the coalescing rule.
CREATE UNIQUE INDEX IF NOT EXISTS ref_view_builds_single_inflight
    ON ref_view_builds(ref_view_id, desired_tree, base_generation_id, build_fingerprint)
    WHERE state = 'building';

-- Deferred deletion work. Targets are recorded as an opaque encoded list,
-- never as foreign keys: the whole point of the journal is to survive the
-- disappearance of the rows it names.
CREATE TABLE IF NOT EXISTS cleanup_journal (
    cleanup_id        TEXT PRIMARY KEY,
    opaque_target_ids TEXT NOT NULL,
    reason            TEXT NOT NULL,
    phase             TEXT NOT NULL,
    grace_deadline    INTEGER NOT NULL DEFAULT 0,
    primary_epoch     INTEGER NOT NULL DEFAULT 0,
    last_progress     INTEGER NOT NULL DEFAULT 0,
    last_error        TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
`

const vectorTableSQL = `
CREATE TABLE IF NOT EXISTS vectors (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    parent_id   TEXT NOT NULL DEFAULT '',
    dims        INTEGER NOT NULL,
    vec         BLOB NOT NULL
) WITHOUT ROWID;
`

const vectorRepoIndexSQL = `CREATE INDEX IF NOT EXISTS vectors_by_repo ON vectors(repo_prefix, node_id)`

// schemaSQL is the canonical DDL applied on Open. Statements are
// idempotent (IF NOT EXISTS) so they run cleanly against a fresh DB
// and against an existing one.
//
// Schema choices
//
//   - nodes.id is the primary key; INSERT OR REPLACE on the id column
//     gives idempotent re-adds with last-write-wins on every other
//     column, matching the in-memory store's behaviour.
//
//   - edges has a synthetic INTEGER PRIMARY KEY plus a UNIQUE
//     constraint over (from_id, to_id, kind, file_path, line) -- the
//     logical edge key the in-memory store uses for dedup. INSERT OR
//     IGNORE on that constraint matches the in-memory "second AddEdge
//     for the same key is a no-op" semantics.
//
//   - meta is a JSON document (see meta_json.go). nil / empty Meta is
//     stored as NULL. Four universal, hot-read node keys are promoted to
//     their own nullable columns (signature / visibility / doc /
//     external): they are stripped from the JSON blob on write and
//     restored into Meta on read, so the in-memory map is unchanged. A
//     NULL column means "not set" (legacy gob rows predate the columns
//     and keep their values in the blob). Existing databases gain the
//     columns via ALTER on the next Open (ensureNodeColumns).
//
//   - Secondary indexes mirror the in-memory store's hot lookup paths:
//     nodes_by_name      -- FindNodesByName / FindNodesByNameInRepo
//     nodes_by_kind      -- Stats (group-by-kind)
//     nodes_by_file      -- GetFileNodes, EvictFile
//     nodes_by_repo      -- GetRepoNodes, RepoStats, EvictRepo
//     (partial index -- empty repo_prefix is
//     the common case and indexing it would
//     be pure overhead)
//     nodes_by_qual      -- GetNodeByQualName; non-unique because a
//     language-level identity is not a global graph identity
//     edges_by_from      -- GetOutEdges (kind included so RemoveEdge
//     can probe by (from, kind) without a
//     second hop)
//     edges_by_to        -- GetInEdges
const schemaSQL = `
CREATE TABLE IF NOT EXISTS nodes (
    id            TEXT PRIMARY KEY,
    kind          TEXT NOT NULL,
    name          TEXT NOT NULL,
    qual_name     TEXT NOT NULL DEFAULT '',
    file_path     TEXT NOT NULL,
    start_line    INTEGER NOT NULL DEFAULT 0,
    end_line      INTEGER NOT NULL DEFAULT 0,
    start_column  INTEGER NOT NULL DEFAULT 0,
    end_column    INTEGER NOT NULL DEFAULT 0,
    language      TEXT NOT NULL DEFAULT '',
    repo_prefix   TEXT NOT NULL DEFAULT '',
    workspace_id  TEXT NOT NULL DEFAULT '',
    project_id    TEXT NOT NULL DEFAULT '',
    signature     TEXT,
    visibility    TEXT,
    doc           TEXT,
    external      INTEGER,
    return_type   TEXT,
    is_async      INTEGER,
    is_static     INTEGER,
    is_abstract   INTEGER,
    is_exported   INTEGER,
    updated_at    INTEGER,
    data_class    TEXT,
    clone_sig     TEXT,
    meta          BLOB
) WITHOUT ROWID;

-- nodes_by_name / _kind / _file / _repo are created from the shared
-- bulkDroppableIndexes set (see bulk_load.go), not here, so the bulk-load
-- fast path can drop and rebuild the EXACT same DDL without drift.
-- Qualified names are language-level lookup labels, not graph identities:
-- forks, worktrees, manifest overlays and independent repositories may emit
-- the same value. Keep this index non-unique and let repository/workspace-aware
-- callers disambiguate candidates.
CREATE INDEX IF NOT EXISTS nodes_by_qual ON nodes(qual_name) WHERE qual_name <> '';

CREATE TABLE IF NOT EXISTS edges (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    from_id          TEXT NOT NULL,
    to_id            TEXT NOT NULL,
    kind             TEXT NOT NULL,
    file_path        TEXT NOT NULL DEFAULT '',
    line             INTEGER NOT NULL DEFAULT 0,
    confidence       REAL NOT NULL DEFAULT 1.0,
    confidence_label TEXT NOT NULL DEFAULT '',
    origin           TEXT NOT NULL DEFAULT '',
    tier             TEXT NOT NULL DEFAULT '',
    cross_repo       INTEGER NOT NULL DEFAULT 0,
    meta             BLOB,
    UNIQUE(from_id, to_id, kind, file_path, line)
);

-- edges_by_from / _to / _kind are created from the shared
-- bulkDroppableIndexes set (see bulk_load.go), not here, so the bulk-load
-- fast path can drop and rebuild the EXACT same DDL without drift.
-- edges_by_kind backs EdgesByKind / EdgesByKinds (resolver whole-graph
-- passes probe single kinds like provides/imports on every file save);
-- without it those are full edges-table scans — edges_by_from/to lead
-- with an id column and the partial edges_external index only covers
-- its own predicate.

CREATE TABLE IF NOT EXISTS file_mtimes (
    repo_prefix TEXT NOT NULL,
    file_path   TEXT NOT NULL,
    mtime_ns    INTEGER NOT NULL,
    PRIMARY KEY (repo_prefix, file_path)
) WITHOUT ROWID;

-- repo_index_state records per-repo freshness provenance written at the
-- end of a (re)index: the git revision + dirty flag the graph reflects,
-- the Merkle workspace fingerprint (Tree.Root) that gates global-pass
-- short-circuiting, node/edge counts for the index-plausibility baseline,
-- and the JSON per-language extractor versions that produced the graph.
-- One row per repo_prefix; WITHOUT ROWID — the PK index IS the table,
-- like file_mtimes / clone_shingles.
CREATE TABLE IF NOT EXISTS repo_index_state (
    repo_prefix        TEXT PRIMARY KEY,
    indexed_sha        TEXT NOT NULL DEFAULT '',
    dirty              INTEGER NOT NULL DEFAULT 0,
    indexed_at         INTEGER NOT NULL DEFAULT 0,
    workspace_fp       TEXT NOT NULL DEFAULT '',
    node_count         INTEGER NOT NULL DEFAULT 0,
    edge_count         INTEGER NOT NULL DEFAULT 0,
    extractor_versions TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- enrichment_state records, per (repo, semantic provider), the git revision
-- the graph was enriched at plus the coverage that pass reached. Enrichment
-- completion otherwise lives only in an in-memory map, so a restart forgets it
-- and re-runs full LSP hover passes for a repo whose persisted graph already
-- carries the edges. The deferred-enrichment gate reads this row and skips a
-- provider whose IndexedSHA still matches HEAD on a clean tree. One row per
-- (repo_prefix, provider); WITHOUT ROWID — the PK index IS the table, like
-- file_mtimes / repo_index_state.
CREATE TABLE IF NOT EXISTS enrichment_state (
    repo_prefix  TEXT NOT NULL,
    provider     TEXT NOT NULL,
    indexed_sha  TEXT NOT NULL DEFAULT '',
    completed_at INTEGER NOT NULL DEFAULT 0,
    coverage     REAL NOT NULL DEFAULT 0,
    PRIMARY KEY (repo_prefix, provider)
) WITHOUT ROWID;

-- contract_state records, per repo, that a WHOLE-REPO contract pass committed
-- against this store: the revision it ran at, when it finished, and how many
-- contracts it wrote. Contracts are committed all-at-once by the tail of a
-- full index while re-index admission is per-file mtime, so a run whose tail
-- is lost leaves the contract tier empty and every later warm restart sees
-- unchanged mtimes and re-extracts nothing. Absent this row the empty tier is
-- indistinguishable from a repo that genuinely declares no contracts; the
-- contract / route query path reads it to say which one it is answering. One
-- row per repo_prefix; WITHOUT ROWID — the PK index IS the table, like
-- file_mtimes / repo_index_state.
CREATE TABLE IF NOT EXISTS contract_state (
    repo_prefix    TEXT PRIMARY KEY,
    indexed_sha    TEXT NOT NULL DEFAULT '',
    completed_at   INTEGER NOT NULL DEFAULT 0,
    contract_count INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;

-- clone_shingles is the per-symbol MinHash shingle-set sidecar. Each
-- function/method node's []uint64 shingle set is stored as a little-
-- endian BLOB (8 bytes/elem) keyed by node_id so the maintained clone-
-- detection count-min sketch can be rebuilt after a warm restart from
-- the snapshot instead of re-parsing every body. repo_prefix carries
-- the owning repo so per-repo reseeds (SELECT … WHERE repo_prefix = ?)
-- and per-repo wipes don't clobber other repos' shingle sets. node_id
-- is the PK (the join key back to nodes.id); like file_mtimes this is a
-- WITHOUT ROWID sidecar so the PK index IS the table.
CREATE TABLE IF NOT EXISTS clone_shingles (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    shingles    BLOB,
    signature   TEXT,
    token_count INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS clone_shingles_by_repo
    ON clone_shingles(repo_prefix, node_id);

-- clone_corpus_state records that one repository's clone sidecar is
-- authoritative even when it contains zero rows. Without this marker an empty
-- page is indistinguishable from a database written before clone_shingles
-- existed, forcing a full GetRepoNodes compatibility scan on every restart.
CREATE TABLE IF NOT EXISTS clone_corpus_state (
    repo_prefix TEXT PRIMARY KEY
) WITHOUT ROWID;

-- constant_values is the per-KindConstant literal-value sidecar: one row
-- per constant whose RHS is a string / numeric literal, keyed by node_id
-- (the join key back to nodes.id). Lifting the value out of the JSON Meta
-- blob keeps it queryable (and out of the every-node-load decode path) so
-- the resolver can dereference a const-identifier dispatch name to its
-- value across files. file_path scopes per-file eviction on reindex;
-- repo_prefix scopes per-repo wipes. WITHOUT ROWID — the PK index IS the
-- table, like file_mtimes / clone_shingles.
CREATE TABLE IF NOT EXISTS constant_values (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT '',
    value       TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

CREATE INDEX IF NOT EXISTS constant_values_by_file ON constant_values(repo_prefix, file_path);

-- semantic_binding_types is the compact compiler-type sidecar consumed by
-- contract extraction. It replaces retained go/packages programs with one
-- bare type string per source binding. The composite primary key supports
-- exact batched joins by repository, graph-scoped file, line, and name;
-- WITHOUT ROWID makes that primary key the table itself.
CREATE TABLE IF NOT EXISTS semantic_binding_types (
    repo_prefix TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL,
    line        INTEGER NOT NULL DEFAULT 0,
    name        TEXT NOT NULL DEFAULT '',
    type_name   TEXT NOT NULL,
    PRIMARY KEY (repo_prefix, file_path, line, name)
) WITHOUT ROWID;

-- files is the per-file metadata sidecar: one row per indexed file carrying
-- the BLAKE3 content hash (the Merkle leaf), byte size, extracted node count,
-- and a JSON array of parse-error locations. The Merkle tree stays the
-- authoritative change detector; this table is queryable supplementary
-- metadata (index_health reports per-file parse errors + node counts from it).
-- PK is (repo_prefix, file_path) so a reindex replaces the row in place;
-- WITHOUT ROWID — the PK index IS the table, like file_mtimes.
CREATE TABLE IF NOT EXISTS files (
    repo_prefix  TEXT NOT NULL DEFAULT '',
    file_path    TEXT NOT NULL,
    content_hash TEXT NOT NULL DEFAULT '',
    size         INTEGER NOT NULL DEFAULT 0,
    node_count   INTEGER NOT NULL DEFAULT 0,
    errors       TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (repo_prefix, file_path)
) WITHOUT ROWID;
-- files_with_errors backs the index_health "files with parse errors" rollup
-- so it scans only the (usually tiny) set of erroring files, not every row.
CREATE INDEX IF NOT EXISTS files_with_errors ON files(repo_prefix) WHERE errors <> '';

-- ref_facts is the resolved-reference sidecar: one row per reference edge
-- that resolved to a concrete target, recording the target + the provenance
-- tier that resolved it. Denormalized file_path + lang make "all reference
-- facts originating in file X" a single indexed query (the scope unit for
-- incremental re-resolution and the audit/diff surface). repo_prefix scopes
-- per-repo. PK is (repo_prefix, from_id, to_id, kind, line) so re-resolving a
-- file replaces its facts in place; WITHOUT ROWID — the PK index IS the table.
CREATE TABLE IF NOT EXISTS ref_facts (
    repo_prefix TEXT NOT NULL DEFAULT '',
    from_id     TEXT NOT NULL,
    to_id       TEXT NOT NULL,
    kind        TEXT NOT NULL,
    ref_name    TEXT NOT NULL DEFAULT '',
    line        INTEGER NOT NULL DEFAULT 0,
    origin      TEXT NOT NULL DEFAULT '',
    tier        TEXT NOT NULL DEFAULT '',
    candidates  TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT '',
    lang        TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (repo_prefix, from_id, to_id, kind, line)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS ref_facts_by_file ON ref_facts(repo_prefix, file_path);
-- ref_facts_by_target backs the reverse lookup ("which files hold a fact
-- resolving TO these symbols") that affected-by re-resolution runs when a
-- file's symbol signatures change. Without it that query is a full
-- ref_facts scan — the PK leads with from_id, not to_id.
CREATE INDEX IF NOT EXISTS ref_facts_by_target ON ref_facts(repo_prefix, to_id);

` + vectorTableSQL + `

-- churn_enrichment is the per-node git-churn sidecar (change A: move
-- enrichment OUT of nodes.meta so the node hot path stops encoding
-- rarely-read data into the blob and get_churn_rate does an indexed read
-- instead of an AllNodes+meta-decode scan). One typed row per enriched
-- file/function/method node, keyed by node_id (join key back to
-- nodes.id); repo_prefix scopes
-- per-repo reseeds/wipes. head_sha/branch/computed_at are file-level only
-- (empty for symbols). WITHOUT ROWID: the PK index IS the table.
CREATE TABLE IF NOT EXISTS churn_enrichment (
    node_id        TEXT PRIMARY KEY,
    repo_prefix    TEXT NOT NULL DEFAULT '',
    commit_count   INTEGER NOT NULL DEFAULT 0,
    age_days       INTEGER NOT NULL DEFAULT 0,
    churn_rate     REAL NOT NULL DEFAULT 0,
    last_author    TEXT NOT NULL DEFAULT '',
    last_commit_at TEXT NOT NULL DEFAULT '',
    head_sha       TEXT NOT NULL DEFAULT '',
    branch         TEXT NOT NULL DEFAULT '',
    computed_at    TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS churn_by_repo ON churn_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- coverage_enrichment: per-symbol coverage sidecar (change A). Typed
-- columns keyed by node_id; repo_prefix scopes per-repo wipes.
CREATE TABLE IF NOT EXISTS coverage_enrichment (
    node_id      TEXT PRIMARY KEY,
    repo_prefix  TEXT NOT NULL DEFAULT '',
    coverage_pct REAL NOT NULL DEFAULT 0,
    num_stmt     INTEGER NOT NULL DEFAULT 0,
    hit          INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS coverage_by_repo ON coverage_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- release_enrichment: per-file "added_in <tag>" sidecar (change A).
CREATE TABLE IF NOT EXISTS release_enrichment (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    added_in    TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS release_by_repo ON release_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- blame_enrichment: per-symbol latest-author sidecar (change A).
CREATE TABLE IF NOT EXISTS blame_enrichment (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    commit_sha  TEXT NOT NULL DEFAULT '',
    email       TEXT NOT NULL DEFAULT '',
    ts          INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS blame_by_repo ON blame_enrichment(repo_prefix) WHERE repo_prefix <> '';

-- symbol_fts is the FTS5 full-text index over pre-tokenised symbol
-- names. It replaces the multi-GB in-heap BM25 index with an
-- on-disk inverted index the SymbolSearcher / SymbolBundleSearcher
-- query through. A standard (NOT contentless) FTS5 table; individual
-- rows are deleted by their FTS5 docid via the symbol_fts_rowid sidecar
-- below (node_id is UNINDEXED, so a DELETE keyed on it would full-scan
-- the index). node_id is the join key back to nodes.id; repo_prefix is
-- carried UNINDEXED so per-repo staleness wipes (DELETE … WHERE
-- repo_prefix = ?) hit a literal column without a separate b-tree.
-- Only "tokens" is indexed for matching. IF NOT EXISTS makes this
-- idempotent on every Open, so an existing .sqlite gains the vtable
-- on its next open + reindex.
CREATE VIRTUAL TABLE IF NOT EXISTS symbol_fts USING fts5(node_id UNINDEXED, repo_prefix UNINDEXED, tokens);

-- symbol_fts_state records which deterministic token-normalization mode built
-- each repository's durable symbol corpus. The marker advances only after an
-- authoritative replacement succeeds, so a crash can cause a harmless repeat
-- rebuild but can never certify a corpus written with a different mode.
CREATE TABLE IF NOT EXISTS symbol_fts_state (
    repo_prefix  TEXT PRIMARY KEY,
    normalization TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;

-- symbol_fts_rowid maps a node_id to the rowid (FTS5 docid) of its row in
-- symbol_fts. node_id is UNINDEXED in the FTS5 vtable, so deleting a node's
-- prior row with "DELETE … WHERE node_id = ?" full-scans the entire index
-- once PER symbol — quadratic on the per-edit reindex hot path. This sidecar
-- turns the delete into an O(log n) docid delete ("WHERE rowid = ?", the FTS5
-- docid IS indexed). One row per indexed symbol, keyed by node_id (the join
-- key back to nodes.id); repo_prefix scopes the per-repo wipe that
-- BulkUpsertSymbolFTS performs in lockstep with symbol_fts. WITHOUT ROWID:
-- the PK index IS the table, like file_mtimes / clone_shingles.
CREATE TABLE IF NOT EXISTS symbol_fts_rowid (
    node_id     TEXT PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    fts_rowid   INTEGER NOT NULL
) WITHOUT ROWID;
CREATE UNIQUE INDEX IF NOT EXISTS symbol_fts_rowid_by_rowid
    ON symbol_fts_rowid(fts_rowid);
CREATE INDEX IF NOT EXISTS symbol_fts_rowid_by_repo
    ON symbol_fts_rowid(repo_prefix, fts_rowid);

-- content_fts is the FTS5 full-text index over CONTENT (data_class=
-- "content") section bodies — text / pdf / pptx / xlsx chunks. It is
-- kept SEPARATE from symbol_fts so content text never enters the symbol
-- search or the code-oriented analysis passes: a content-heavy repo of a
-- few hundred large documents explodes into hundreds of thousands of
-- section nodes, and streaming their bodies here (per file, on disk)
-- instead of into symbol_fts + graph nodes keeps the code index and the
-- graph passes bounded. Only "body" is indexed for matching; node_id /
-- repo_prefix / file_path / ordinal ride UNINDEXED. Staleness deletes never
-- filter those virtual-table columns directly: content_fts_rowid below maps
-- their indexed ownership keys to FTS docids, avoiding one virtual-table scan
-- per streamed file.
CREATE VIRTUAL TABLE IF NOT EXISTS content_fts USING fts5(node_id UNINDEXED, repo_prefix UNINDEXED, file_path UNINDEXED, ordinal UNINDEXED, body);

-- content_fts_rowid is the ownership index for content FTS docids. A content
-- file may emit many sections with the same node/ordinal across append calls,
-- so the FTS docid itself is the primary key; repository/file indexes make
-- whole-repo, per-file, and end-of-walk stale deletes set-oriented. AppendContent
-- assigns explicit FTS rowids and writes this sidecar in the same transaction.
CREATE TABLE IF NOT EXISTS content_fts_rowid (
    fts_rowid   INTEGER PRIMARY KEY,
    repo_prefix TEXT NOT NULL DEFAULT '',
    file_path   TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS content_fts_rowid_by_repo_file
    ON content_fts_rowid(repo_prefix, file_path, fts_rowid);
CREATE INDEX IF NOT EXISTS content_fts_rowid_by_file
    ON content_fts_rowid(file_path, fts_rowid);
` + analysisGenerationSchemaSQL + checkoutCatalogSchemaSQL
