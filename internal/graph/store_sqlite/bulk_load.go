package store_sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/zzet/gortex/internal/graph"
)

// Compile-time assertion: *Store satisfies graph.BulkLoader.
var _ graph.BulkLoader = (*Store)(nil)

// bulkDroppableIndex is one secondary index the bulk-load fast path drops
// before a first/empty cold index and rebuilds afterward.
type bulkDroppableIndex struct {
	name string
	ddl  string
}

// bulkFinalizeEvent decomposes the formerly opaque cold finalization timer.
// The observer is test-only; production telemetry is emitted to the daemon's
// captured stderr with stable stage/name fields.
type bulkFinalizeEvent struct {
	Stage              string
	Name               string
	Elapsed            time.Duration
	Busy               int
	WALFrames          int
	CheckpointedFrames int
	Err                error
}

func (s *Store) emitBulkFinalizeEvent(event bulkFinalizeEvent) {
	if s.bulkFinalizeObserver != nil {
		s.bulkFinalizeObserver(event)
	}
	if event.Err != nil {
		log.Printf("store_sqlite: bulk finalize stage=%s name=%s elapsed=%s error=%q", event.Stage, event.Name, event.Elapsed, event.Err)
		return
	}
	if event.Stage == "checkpoint" {
		log.Printf("store_sqlite: bulk finalize stage=%s name=%s elapsed=%s busy=%d wal_frames=%d checkpointed_frames=%d", event.Stage, event.Name, event.Elapsed, event.Busy, event.WALFrames, event.CheckpointedFrames)
		return
	}
	log.Printf("store_sqlite: bulk finalize stage=%s name=%s elapsed=%s", event.Stage, event.Name, event.Elapsed)
}

// FTS5's merge command writes approximately N pages and returns. Unlike
// optimize, its work is bounded independently of corpus size. Correctness does
// not depend on either command because every FTS row is already transactional.
const coldFTSMergePages = 64

// bulkDroppableIndexes is the single source of truth for these index
// definitions. Open creates them (so the initial DB has them), BeginBulkLoad
// drops them by name, and FlushBulk recreates them from the exact same ddl —
// keeping the initial and post-bulk shapes from drifting.
//
// These are exactly the standalone, NON-UNIQUE CREATE INDEX statements over
// the large nodes / edges tables. Maintaining them per-row across a
// multi-hundred-thousand-row cold load is pure overhead when the rows land
// once, so they are dropped up front and rebuilt in one pass at the end.
//
// Deliberately excluded:
//   - nodes_by_qual: resolver lookups use INDEXED BY and must fail closed
//     rather than scan the full nodes table. Keeping the compact partial index
//     live preserves that contract during every bulk-load phase.
//   - the edges UNIQUE(from_id, …) table constraint and every WITHOUT ROWID
//     primary-key index: not standalone indexes; they cannot be dropped while
//     the table/constraint exists.
//   - edges_external (partial): a tiny index over external-call terminals,
//     created from a shared predicate in Open; not worth dropping.
//
// Dropping/recreating these is a runtime operation on identical DDL — it is
// NOT a schema change, so it does not touch the persisted schema version.
var bulkDroppableIndexes = []bulkDroppableIndex{
	{"nodes_by_name", `CREATE INDEX IF NOT EXISTS nodes_by_name ON nodes(name)`},
	{"nodes_by_kind", `CREATE INDEX IF NOT EXISTS nodes_by_kind ON nodes(kind)`},
	{"nodes_by_file", `CREATE INDEX IF NOT EXISTS nodes_by_file ON nodes(file_path)`},
	{"nodes_by_repo", `CREATE INDEX IF NOT EXISTS nodes_by_repo ON nodes(repo_prefix) WHERE repo_prefix <> ''`},
	// Repo-first (repo_prefix, kind) probes for the repository projections:
	// the flat kind index invites whole-kind-range scans that a repo filter
	// then discards — measured on this workspace at 4.67s vs 0.82s (common
	// kind) and 6.21s vs 0.02s (small repo) against repo-first plans.
	// Deliberately NOT partial: the projections probe repo_prefix through a
	// json_each CTE join, and SQLite cannot prove such a join implies
	// repo_prefix <> '', so a partial index is structurally unusable there —
	// which is precisely why the partial nodes_by_repo never served these
	// queries and they fell back to kind-range scans. WITHOUT ROWID keys
	// make each entry (repo_prefix, kind, id), so ID projections are
	// index-only.
	{"nodes_by_repo_kind", `CREATE INDEX IF NOT EXISTS nodes_by_repo_kind ON nodes(repo_prefix, kind)`},
	// Resolver warmup selects definitions by exact repository, compatible
	// language family, and a bounded page of names. Keep the key minimal: kind
	// is not a query predicate and WITHOUT ROWID secondary indexes already
	// carry the primary-key id. The partial predicate excludes nameless nodes.
	{"nodes_by_repo_language_name", `CREATE INDEX IF NOT EXISTS nodes_by_repo_language_name ON nodes(repo_prefix, language, name) WHERE name <> ''`},
	// Repository-scoped contract/router discovery reads only file-node paths.
	// This partial covering index avoids scanning every symbol in the repo and
	// remains small enough to rebuild cheaply at the end of a cold bulk load.
	{"nodes_repo_files", `CREATE INDEX IF NOT EXISTS nodes_repo_files ON nodes(repo_prefix, workspace_id, language, file_path, id) WHERE kind = 'file'`},
	{"edges_by_from", `CREATE INDEX IF NOT EXISTS edges_by_from ON edges(from_id, kind)`},
	// Site-shaped candidate probes (guard rehydration, resolve-job liveness,
	// edge identity lookups) constrain (from_id, line). Without a line-bearing
	// index the planner satisfies them through the covering WITHOUT-ROWID
	// primary key probed on from_id alone, re-reading the caller's whole
	// out-edge row set per site — hub callers (11k+ out-edges) turn a µs seek
	// into tens of milliseconds, and the cross-package guard alone paid ~980s
	// of a 28-repo cold index that way. With the index the full candidate
	// path measured ~30 → ~116k sites/s on a production store copy.
	{"edges_by_from_line", `CREATE INDEX IF NOT EXISTS edges_by_from_line ON edges(from_id, line)`},
	{"edges_by_to", `CREATE INDEX IF NOT EXISTS edges_by_to ON edges(to_id, kind)`},
	{"edges_by_kind", `CREATE INDEX IF NOT EXISTS edges_by_kind ON edges(kind)`},
	// Exact changed-file frontiers (watcher and partial indexing) must not
	// scan every edge merely to find source sites owned by one file.
	{"edges_by_file", `CREATE INDEX IF NOT EXISTS edges_by_file ON edges(file_path, kind)`},
	// Backs EdgesWithUnresolvedTarget — the resolver's main pending-edge
	// collector, called on every full resolve. is_unresolved is a VIRTUAL
	// generated column (see isUnresolvedColumnDDL); indexing it turns a
	// full-table scan (the prior to_id-based OR query forced SQLite to
	// abandon its index) into an index search whose bookmark lookups land
	// in ascending rowid order (see isUnresolvedColumnDDL's doc comment for
	// why that beats an equivalent-looking to_id-based index).
	// Only unresolved rows belong in the resolver frontier. A dense Boolean
	// index stored one entry for every resolved edge and made cold finalization
	// sort the full edge corpus; the partial index preserves rowid ordering while
	// shrinking rebuild and steady-state maintenance to the pending frontier.
	{"edges_by_unresolved", `CREATE INDEX IF NOT EXISTS edges_by_unresolved ON edges(is_unresolved) WHERE is_unresolved = 1`},
	// The repo-prefixed fn-value placeholder namespace is read once per
	// framework-synthesis pass through a leading-wildcard LIKE that no
	// general index can serve — without this partial index the reader
	// walked every unresolved edge. The reader states the predicate
	// verbatim as its only WHERE clause (the OR-arm implication prover
	// will not bind it otherwise). Near-empty at rest; populated only
	// mid-pass while placeholders await the fn-value gate.
	{"edges_fnvalue_prefixed", `CREATE INDEX IF NOT EXISTS edges_fnvalue_prefixed ON edges(to_id) WHERE to_id LIKE '%::unresolved::fnvalue::%'`},
	// Canonical Go receiver types remain indexed for the SQLite-native repair.
	// The edge side reuses edges_by_kind for its one global cold pass; scoped
	// warm/partial passes continue to seek member_of edges through edges_by_from.
	{"nodes_go_receiver_type", `CREATE INDEX IF NOT EXISTS nodes_go_receiver_type ON nodes(repo_prefix, file_dir, name, id) WHERE language = 'go' AND kind IN ('type', 'interface') AND name <> '' AND file_path <> ''`},
	// Partial index over exactly the not-yet-semantically-stamped nodes per
	// repo. Stays small in steady state (most nodes end up stamped), so a
	// future "unstamped nodes in this repo" query is an index scan over the
	// residual few instead of a full-table decode of every node's meta.
	{"nodes_semantic_pending", `CREATE INDEX IF NOT EXISTS nodes_semantic_pending ON nodes(repo_prefix) WHERE semantic_type IS NULL`},
}

// bulkCacheSizeKiB is the page cache the fast path requests on its pinned
// connection. SQLite reads a negative cache_size as a KiB budget, so this is
// ~256 MiB — large enough to keep the cold load's working set resident.
const bulkCacheSizeKiB = -262144

// beginWrite starts a write transaction. During a bulk-load fast path it pins
// the single connection that carries synchronous=OFF + the enlarged page
// cache (database/sql PRAGMAs are connection-local, so a pooled connection
// would not see them); otherwise it uses the shared pool. The caller holds
// writeMu, which also guards s.bulkConn.
func (s *Store) beginWrite() (*sql.Tx, error) {
	return s.beginWriteContext(context.Background())
}

type sqliteTxBeginner interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func (s *Store) beginWriteContext(ctx context.Context) (*sql.Tx, error) {
	if s.bulkConn != nil {
		return s.beginWriteOnConnContext(ctx, s.bulkConn)
	}
	return s.beginWriteOnContext(ctx, s.writerDB)
}

func (s *Store) beginWriteOnConnContext(ctx context.Context, conn *sql.Conn) (*sql.Tx, error) {
	return s.beginWriteOnContext(ctx, conn)
}

func (s *Store) beginWriteOnContext(ctx context.Context, beginner sqliteTxBeginner) (*sql.Tx, error) {
	var tx *sql.Tx
	err := s.withSQLiteBusyRetry(ctx, "begin_write", func(attemptCtx context.Context) error {
		var beginErr error
		tx, beginErr = beginner.BeginTx(attemptCtx, nil)
		return beginErr
	})
	return tx, err
}

// execActiveWriteLocked and queryActiveWriteLocked keep sidecar and eviction
// writes on the pinned bulk connection when one is active. Callers hold
// writeMu, which guards bulkConn for the full operation.
func (s *Store) execActiveWriteLocked(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if s.bulkConn != nil {
		return s.bulkConn.ExecContext(ctx, query, args...)
	}
	return s.writerDB.ExecContext(ctx, query, args...)
}

func (s *Store) queryActiveWriteLocked(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if s.bulkConn != nil {
		return s.bulkConn.QueryContext(ctx, query, args...)
	}
	return s.writerDB.QueryContext(ctx, query, args...)
}

// activeWriteConnLocked returns the sole writer connection while writeMu is
// held. A cold bulk window already pins that connection, so callers must reuse
// it and the returned release is a no-op; otherwise release returns the checked
// out writer connection to its max-one pool.
func (s *Store) activeWriteConnLocked(ctx context.Context) (*sql.Conn, func(), error) {
	if s.bulkConn != nil {
		return s.bulkConn, func() {}, nil
	}
	conn, err := s.writerDB.Conn(ctx)
	if err != nil {
		return nil, nil, err
	}
	return conn, func() { _ = conn.Close() }, nil
}

// BeginBulkLoad enters the bulk-load fast path for a first/empty cold index.
// It pins one connection at synchronous=OFF with an enlarged page cache and
// drops the droppable secondary indexes, so a multi-hundred-thousand-row load
// skips per-row B-tree maintenance and per-commit fsync. FlushBulk reverses
// all of it: restore the pragmas, rebuild the indexes, and checkpoint.
//
// Gated: it engages ONLY when the nodes table is empty. On a populated store
// (incremental reindex, warm restart, or a later repo in a multi-repo cold
// start that shares the disk store) it is a safe no-op — dropping indexes or
// disabling crash durability under live, concurrently-readable rows would be
// unsafe. In-memory stores have no WAL / on-disk B-tree pressure, so it is a
// no-op there too.
func (s *Store) BeginBulkLoad() {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.coordinatedBulkLoad {
		return
	}
	s.beginBulkLoadLocked()
}

// BeginCoordinatedBulkLoad opens one outer cold-load window around a set of
// concurrently indexed repositories. It returns true only when the store was
// empty and the fast path engaged. While active, the ordinary per-repository
// BeginBulkLoad/FlushBulk pair is a no-op: every shadow drain still routes its
// writes through the pinned connection, but secondary indexes rebuild once at
// EndCoordinatedBulkLoad instead of after the first repository.
func (s *Store) BeginCoordinatedBulkLoad() bool {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.coordinatedBulkLoad || s.bulkConn != nil {
		return false
	}
	s.beginBulkLoadLocked()
	if s.bulkConn == nil {
		return false
	}
	s.coordinatedBulkLoad = true
	return true
}

func (s *Store) beginBulkLoadLocked() {

	// Re-entrancy / non-disk guard: a second BeginBulkLoad without an
	// intervening FlushBulk, or an in-memory store, stays a no-op.
	if s.bulkConn != nil || isMemoryPath(s.dbPath) {
		return
	}

	ctx := context.Background()
	conn, err := s.writerDB.Conn(ctx)
	if err != nil {
		return
	}

	// Gate to a genuinely first/empty index.
	if !nodesTableEmpty(ctx, conn) {
		_ = conn.Close()
		return
	}

	// Capture prior pragma values so FlushBulk (and every early-return /
	// error path) can restore them. If they can't be read, don't engage —
	// a slow correct load beats a connection stuck at synchronous=OFF.
	prevSync, err := pragmaInt(ctx, conn, "synchronous")
	if err != nil {
		_ = conn.Close()
		return
	}
	prevCache, err := pragmaInt(ctx, conn, "cache_size")
	if err != nil {
		_ = conn.Close()
		return
	}

	// synchronous=OFF drops crash durability for the load window —
	// acceptable only because a crash on a fresh index just re-indexes.
	if _, err := conn.ExecContext(ctx, "PRAGMA synchronous = OFF"); err != nil {
		_ = conn.Close()
		return
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size = %d", bulkCacheSizeKiB)); err != nil {
		// Roll the durability change back before bailing.
		_, _ = conn.ExecContext(ctx, fmt.Sprintf("PRAGMA synchronous = %d", prevSync))
		_ = conn.Close()
		return
	}

	// Drop the droppable secondary indexes; rebuilt in one pass at
	// FlushBulk. Best-effort: a failed drop just means that index keeps
	// being maintained per-row (slower, still correct), so it is not fatal.
	for _, idx := range bulkDroppableIndexes {
		_, _ = conn.ExecContext(ctx, "DROP INDEX IF EXISTS "+idx.name)
	}

	s.bulkConn = conn
	s.bulkPrevSync = prevSync
	s.bulkPrevCacheSize = prevCache
	// The bulk path changes durability and secondary-index maintenance outside
	// the ordinary row mutation protocol. Active receipts therefore fail closed.
	s.markMutationReceiptsIncompleteLocked()
}

// FlushBulk exits the bulk-load fast path: it rebuilds every dropped index,
// restores synchronous + cache_size, releases the pinned writer and write gate,
// then performs one bounded TRUNCATE checkpoint. The ordering matters: the
// durability checkpoint must run on a NORMAL connection, and it must not wait
// for a second writer slot while the bulk connection is still pinned.
func (s *Store) FlushBulk() error {
	s.writeMu.Lock()
	if s.coordinatedBulkLoad {
		s.writeMu.Unlock()
		return nil
	}
	hadBulk := s.bulkConn != nil
	flushErr := s.flushBulkLocked()
	s.writeMu.Unlock()
	if !hadBulk {
		return flushErr
	}
	return errors.Join(flushErr, s.checkpointBulkWAL())
}

// EndCoordinatedBulkLoad closes an outer multi-repository cold-load window.
// It is idempotent and safe to defer: durability/cache restoration and index
// rebuild happen even when one repository failed or panicked.
func (s *Store) EndCoordinatedBulkLoad() error {
	s.writeMu.Lock()
	if !s.coordinatedBulkLoad {
		s.writeMu.Unlock()
		return nil
	}
	s.coordinatedBulkLoad = false
	if s.deferredFTSOptimize {
		// A full FTS5 optimize is unbounded and previously sat directly on the
		// cold-start critical path. One bounded merge keeps segment growth in
		// check; the index is already transactionally correct without either.
		started := time.Now()
		_, err := s.execActiveWriteLocked(context.Background(), `INSERT INTO symbol_fts(symbol_fts, rank) VALUES('merge', ?)`, coldFTSMergePages)
		s.emitBulkFinalizeEvent(bulkFinalizeEvent{Stage: "fts_merge", Name: "symbol_fts", Elapsed: time.Since(started), Err: err})
		s.deferredFTSOptimize = false
	}
	if s.deferredContentFTS {
		started := time.Now()
		_, err := s.execActiveWriteLocked(context.Background(), `INSERT INTO content_fts(content_fts, rank) VALUES('merge', ?)`, coldFTSMergePages)
		s.emitBulkFinalizeEvent(bulkFinalizeEvent{Stage: "fts_merge", Name: "content_fts", Elapsed: time.Since(started), Err: err})
		s.deferredContentFTS = false
	}
	hadBulk := s.bulkConn != nil
	flushErr := s.flushBulkLocked()
	// Refresh planner statistics while the write lock is still held and the
	// droppable indexes are freshly rebuilt: every post-load phase (resolver,
	// enrichment, global passes) plans against this store, and a store
	// without sqlite_stat1 rows plans blind (see refreshPlannerStatsLocked).
	statsStarted := time.Now()
	statsErr := s.refreshPlannerStatsLocked(context.Background())
	s.emitBulkFinalizeEvent(bulkFinalizeEvent{Stage: "planner_stats", Name: "analyze", Elapsed: time.Since(statsStarted), Err: statsErr})
	s.writeMu.Unlock()
	if !hadBulk {
		return flushErr
	}
	return errors.Join(flushErr, s.checkpointBulkWAL())
}

// flushBulkLocked rebuilds indexes and restores the pinned connection. It does
// not checkpoint: callers must first release both the physical connection and
// writeMu, then call checkpointBulkWAL.
func (s *Store) flushBulkLocked() (retErr error) {
	conn := s.bulkConn
	if conn == nil {
		return nil
	}
	// Detach first: the fast path is over regardless of the outcome below.
	s.bulkConn = nil
	// A receipt may have started after BeginBulkLoad. Rebuilding the dropped
	// indexes is outside the ordinary row protocol, so that window also fails
	// closed even when restoration later returns an error.
	s.markMutationReceiptsIncompleteLocked()

	ctx := context.Background()
	defer func() {
		// Restore durability before the connection can return to the max-one
		// writer pool. The final checkpoint only starts after this defer closes
		// the pinned handle and the caller releases writeMu.
		_, syncErr := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA synchronous = %d", s.bulkPrevSync))
		_, cacheErr := conn.ExecContext(ctx, fmt.Sprintf("PRAGMA cache_size = %d", s.bulkPrevCacheSize))
		closeErr := conn.Close()
		retErr = errors.Join(
			retErr,
			wrapBulkRestoreError("synchronous", syncErr),
			wrapBulkRestoreError("cache_size", cacheErr),
			closeErr,
		)
	}()

	for _, idx := range bulkDroppableIndexes {
		started := time.Now()
		_, err := conn.ExecContext(ctx, idx.ddl)
		s.emitBulkFinalizeEvent(bulkFinalizeEvent{Stage: "index", Name: idx.name, Elapsed: time.Since(started), Err: err})
		if err != nil {
			return fmt.Errorf("store_sqlite: rebuild index %s: %w", idx.name, err)
		}
	}
	return nil
}

func wrapBulkRestoreError(pragma string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("store_sqlite: restore bulk PRAGMA %s: %w", pragma, err)
}

func (s *Store) checkpointBulkWAL() error {
	ctx, cancel := context.WithTimeout(context.Background(), walCheckpointTimeout)
	defer cancel()
	started := time.Now()
	result, err := s.checkpointWALWithContextResult(ctx)
	s.emitBulkFinalizeEvent(bulkFinalizeEvent{
		Stage: "checkpoint", Name: "wal_truncate", Elapsed: time.Since(started),
		Busy: result.Busy, WALFrames: result.WALFrames,
		CheckpointedFrames: result.CheckpointedFrames, Err: err,
	})
	if err != nil {
		return fmt.Errorf("store_sqlite: bulk checkpoint: %w", err)
	}
	return nil
}

// nodesTableEmpty reports whether the nodes table holds no rows. Used to gate
// the bulk-load fast path to a genuinely first/empty cold index.
func nodesTableEmpty(ctx context.Context, conn *sql.Conn) bool {
	var one int
	err := conn.QueryRowContext(ctx, "SELECT 1 FROM nodes LIMIT 1").Scan(&one)
	return errors.Is(err, sql.ErrNoRows)
}

// pragmaInt reads a single-integer PRAGMA (synchronous, cache_size) off the
// given connection.
func pragmaInt(ctx context.Context, conn *sql.Conn, pragma string) (int64, error) {
	var v int64
	if err := conn.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&v); err != nil {
		return 0, err
	}
	return v, nil
}
