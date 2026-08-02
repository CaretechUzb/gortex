package resolver

import (
	"database/sql"
	"fmt"
	"os"
	"strings"

	"github.com/zzet/gortex/internal/graph"
	_ "modernc.org/sqlite"
)

type lspSpoolRecord struct {
	key       deferredLSPWorkKey
	currentTo string
	carried   bool
	payload   persistedEdgeSnapshot
}

// deferredLSPSpool is the disk-backed continuation queue for whole-pass LSP
// work. Its primary key is the same stable source identity the old Go map used;
// INSERT ... ON CONFLICT preserves exact dedup while sorted keyset pages bound
// both current work and budget-skipped retries.
type deferredLSPSpool struct {
	db   *sql.DB
	path string
}

func newDeferredLSPSpool() (*deferredLSPSpool, error) {
	file, err := os.CreateTemp("", "gortex-resolve-lsp-*.sqlite")
	if err != nil {
		return nil, err
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.Exec(`PRAGMA journal_mode=WAL;
PRAGMA synchronous=NORMAL;
CREATE TABLE work (
  file_path TEXT NOT NULL,
  line INTEGER NOT NULL,
  from_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  target TEXT NOT NULL,
  current_to TEXT NOT NULL,
  carried INTEGER NOT NULL DEFAULT 0,
  confidence REAL NOT NULL,
  confidence_label TEXT NOT NULL,
  origin TEXT NOT NULL,
  tier TEXT NOT NULL,
  cross_repo INTEGER NOT NULL,
  meta BLOB,
  PRIMARY KEY(file_path, line, from_id, kind, target)
) WITHOUT ROWID;`); err != nil {
		_ = db.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return &deferredLSPSpool{db: db, path: path}, nil
}

func (s *deferredLSPSpool) close() {
	if s == nil {
		return
	}
	if s.db != nil {
		_ = s.db.Close()
	}
	_ = os.Remove(s.path)
	_ = os.Remove(s.path + "-wal")
	_ = os.Remove(s.path + "-shm")
}

func (s *deferredLSPSpool) append(edges []deferredLSPEdge) error {
	if s == nil || len(edges) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	const chunkSize = 400
	for start := 0; start < len(edges); start += chunkSize {
		end := start + chunkSize
		if end > len(edges) {
			end = len(edges)
		}
		values := make([]string, 0, end-start)
		args := make([]any, 0, (end-start)*13)
		for _, deferred := range edges[start:end] {
			if deferred.edge == nil {
				continue
			}
			key := deferredLSPWorkKeyFor(deferred)
			payload := snapshotPersistedEdge(deferred.edge)
			values = append(values, "(?,?,?,?,?,?,?,?,?,?,?,?,?)")
			args = append(args, key.filePath, key.line, key.from, string(key.kind), key.target,
				deferred.edge.To, boolInt(deferred.carried), payload.confidence,
				payload.confidenceLabel, payload.origin, payload.tier, boolInt(payload.crossRepo), payload.meta)
		}
		if len(values) == 0 {
			continue
		}
		query := `INSERT INTO work(
  file_path,line,from_id,kind,target,current_to,carried,
  confidence,confidence_label,origin,tier,cross_repo,meta
) VALUES ` + strings.Join(values, ",") + `
ON CONFLICT(file_path,line,from_id,kind,target) DO UPDATE SET
  current_to=excluded.current_to, carried=excluded.carried,
  confidence=excluded.confidence, confidence_label=excluded.confidence_label,
  origin=excluded.origin, tier=excluded.tier, cross_repo=excluded.cross_repo,
  meta=excluded.meta`
		if _, err := tx.Exec(query, args...); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func (s *deferredLSPSpool) count() int {
	if s == nil {
		return 0
	}
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM work`).Scan(&count)
	return count
}

func (s *deferredLSPSpool) hasForScope(scope map[string]struct{}) bool {
	if s == nil {
		return false
	}
	if len(scope) == 0 {
		return s.count() > 0
	}
	rows, err := s.db.Query(`SELECT from_id, current_to, kind, file_path, line FROM work`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var from, to, kind, filePath string
		var line int
		if rows.Scan(&from, &to, &kind, &filePath, &line) != nil {
			continue
		}
		edge := &graph.Edge{From: from, To: to, Kind: graph.EdgeKind(kind), FilePath: filePath, Line: line}
		if edgeInResolveScope(edge, scope) {
			return true
		}
	}
	return false
}

type deferredLSPSpoolIter struct {
	spool   *deferredLSPSpool
	start   *deferredLSPWorkKey
	after   *deferredLSPWorkKey
	wrapped bool
	done    bool
}

func (s *deferredLSPSpool) iterator(start *deferredLSPWorkKey) *deferredLSPSpoolIter {
	return &deferredLSPSpoolIter{spool: s, start: start}
}

func (it *deferredLSPSpoolIter) next(limit int) ([]lspSpoolRecord, bool, error) {
	if it.done || it.spool == nil {
		return nil, true, nil
	}
	if limit <= 0 {
		limit = resolvePendingPageRows
	}
	for {
		where, args := it.bounds()
		args = append(args, limit)
		rows, err := it.spool.db.Query(`SELECT
  file_path,line,from_id,kind,target,current_to,carried,
  confidence,confidence_label,origin,tier,cross_repo,meta
FROM work `+where+`
ORDER BY file_path,line,from_id,kind,target LIMIT ?`, args...)
		if err != nil {
			return nil, false, err
		}
		records := make([]lspSpoolRecord, 0, limit)
		for rows.Next() {
			var record lspSpoolRecord
			var kind string
			var carried, crossRepo int
			if err := rows.Scan(&record.key.filePath, &record.key.line, &record.key.from,
				&kind, &record.key.target, &record.currentTo, &carried,
				&record.payload.confidence, &record.payload.confidenceLabel,
				&record.payload.origin, &record.payload.tier, &crossRepo, &record.payload.meta); err != nil {
				_ = rows.Close()
				return nil, false, err
			}
			record.key.kind = graph.EdgeKind(kind)
			record.carried = carried != 0
			record.payload.valid = true
			record.payload.from = record.key.from
			record.payload.to = record.currentTo
			record.payload.kind = record.key.kind
			record.payload.filePath = record.key.filePath
			record.payload.line = record.key.line
			record.payload.crossRepo = crossRepo != 0
			records = append(records, record)
		}
		if err := rows.Close(); err != nil {
			return nil, false, err
		}
		if len(records) > 0 {
			last := records[len(records)-1].key
			it.after = &last
			return records, false, nil
		}
		if it.start != nil && !it.wrapped {
			it.wrapped = true
			it.after = nil
			continue
		}
		it.done = true
		return nil, true, nil
	}
}

func (it *deferredLSPSpoolIter) bounds() (string, []any) {
	tuple := `(file_path,line,from_id,kind,target)`
	keyArgs := func(key *deferredLSPWorkKey) []any {
		return []any{key.filePath, key.line, key.from, string(key.kind), key.target}
	}
	if it.start == nil {
		if it.after == nil {
			return "", nil
		}
		return "WHERE " + tuple + " > (?,?,?,?,?)", keyArgs(it.after)
	}
	if !it.wrapped {
		if it.after == nil {
			return "WHERE " + tuple + " >= (?,?,?,?,?)", keyArgs(it.start)
		}
		return "WHERE " + tuple + " > (?,?,?,?,?)", keyArgs(it.after)
	}
	if it.after == nil {
		return "WHERE " + tuple + " < (?,?,?,?,?)", keyArgs(it.start)
	}
	args := append(keyArgs(it.after), keyArgs(it.start)...)
	return "WHERE " + tuple + " > (?,?,?,?,?) AND " + tuple + " < (?,?,?,?,?)", args
}

// keysPage reads one key-only page in spool order within (after, before) —
// nil bounds are open. The expensive-path drain uses it instead of the full
// iterator: no payload columns cross SQLite, no record decode, just the PK
// tuple straight off the primary index.
func (s *deferredLSPSpool) keysPage(after, before *deferredLSPWorkKey, limit int) ([]deferredLSPWorkKey, bool, error) {
	if limit <= 0 {
		limit = resolvePendingPageRows
	}
	tuple := `(file_path,line,from_id,kind,target)`
	where := ""
	var args []any
	appendKey := func(key *deferredLSPWorkKey) {
		args = append(args, key.filePath, key.line, key.from, string(key.kind), key.target)
	}
	switch {
	case after != nil && before != nil:
		where = "WHERE " + tuple + " > (?,?,?,?,?) AND " + tuple + " < (?,?,?,?,?)"
		appendKey(after)
		appendKey(before)
	case after != nil:
		where = "WHERE " + tuple + " > (?,?,?,?,?)"
		appendKey(after)
	case before != nil:
		where = "WHERE " + tuple + " < (?,?,?,?,?)"
		appendKey(before)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT file_path,line,from_id,kind,target FROM work `+where+`
ORDER BY file_path,line,from_id,kind,target LIMIT ?`, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	keys := make([]deferredLSPWorkKey, 0, limit)
	for rows.Next() {
		var key deferredLSPWorkKey
		var kind string
		if err := rows.Scan(&key.filePath, &key.line, &key.from, &kind, &key.target); err != nil {
			return nil, false, err
		}
		key.kind = graph.EdgeKind(kind)
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return keys, len(keys) < limit, nil
}

// markCarriedRange sets carried=1 for every row in (after, before) — nil
// bounds are open — as ONE statement, so a drained segment pays a single
// range update instead of page-by-page tuple lists.
func (s *deferredLSPSpool) markCarriedRange(after, before *deferredLSPWorkKey) error {
	tuple := `(file_path,line,from_id,kind,target)`
	where := ""
	var args []any
	appendKey := func(key *deferredLSPWorkKey) {
		args = append(args, key.filePath, key.line, key.from, string(key.kind), key.target)
	}
	switch {
	case after != nil && before != nil:
		where = " WHERE " + tuple + " > (?,?,?,?,?) AND " + tuple + " < (?,?,?,?,?)"
		appendKey(after)
		appendKey(before)
	case after != nil:
		where = " WHERE " + tuple + " > (?,?,?,?,?)"
		appendKey(after)
	case before != nil:
		where = " WHERE " + tuple + " < (?,?,?,?,?)"
		appendKey(before)
	}
	if _, err := s.db.Exec(`UPDATE work SET carried=1`+where, args...); err != nil {
		return fmt.Errorf("mark carried range: %w", err)
	}
	return nil
}

// lspSpoolRevert names one guard-reverted edge whose spool verify-record
// must follow it back to the unresolved placeholder. Matching includes the
// abandoned bound target so only the record that observed that exact bind is
// refreshed — an exact refresh, not site-level reattachment, preserving the
// spool's precise liveness matching.
type lspSpoolRevert struct {
	edge       *graph.Edge // post-revert state, after both guard batches applied
	oldBoundTo string      // the abandoned resolved target the record observed
}

// refreshRevertedEdges re-snapshots the FULL payload of records whose edges
// the cross-package guard just reverted. The next pass's liveness matching
// (persistedEdgeSnapshot.matches) compares every payload column — target,
// confidence, label, origin, tier, cross_repo, canonical meta — so rewriting
// current_to alone would still leave the row "stale": it must mirror the
// exact post-revert edge state or the queued LSP verify is silently dropped.
func (s *deferredLSPSpool) refreshRevertedEdges(reverts []lspSpoolRevert) error {
	if s == nil || len(reverts) == 0 {
		return nil
	}
	for _, rv := range reverts {
		if rv.edge == nil {
			continue
		}
		snap := snapshotPersistedEdge(rv.edge)
		if _, err := s.db.Exec(
			`UPDATE work SET current_to=?, confidence=?, confidence_label=?, origin=?, tier=?, cross_repo=?, meta=?
  WHERE file_path=? AND line=? AND from_id=? AND kind=? AND current_to=?`,
			snap.to, snap.confidence, snap.confidenceLabel, snap.origin, snap.tier,
			boolInt(snap.crossRepo), snap.meta,
			rv.edge.FilePath, rv.edge.Line, rv.edge.From, string(rv.edge.Kind), rv.oldBoundTo,
		); err != nil {
			return fmt.Errorf("refresh reverted spool records: %w", err)
		}
	}
	return nil
}

func (s *deferredLSPSpool) deleteKeys(keys []deferredLSPWorkKey) error {
	return s.mutateKeys("DELETE FROM work WHERE ", "", keys)
}

func (s *deferredLSPSpool) markCarried(keys []deferredLSPWorkKey) error {
	return s.mutateKeys("UPDATE work SET carried=1 WHERE ", "", keys)
}

func (s *deferredLSPSpool) mutateKeys(prefix, suffix string, keys []deferredLSPWorkKey) error {
	if len(keys) == 0 {
		return nil
	}
	const chunkSize = 512
	for start := 0; start < len(keys); start += chunkSize {
		end := start + chunkSize
		if end > len(keys) {
			end = len(keys)
		}
		values := make([]string, end-start)
		args := make([]any, 0, (end-start)*5)
		for i, key := range keys[start:end] {
			values[i] = "(?,?,?,?,?)"
			args = append(args, key.filePath, key.line, key.from, string(key.kind), key.target)
		}
		query := prefix + `(file_path,line,from_id,kind,target) IN (VALUES ` + strings.Join(values, ",") + `)` + suffix
		if _, err := s.db.Exec(query, args...); err != nil {
			return fmt.Errorf("mutate deferred LSP spool: %w", err)
		}
	}
	return nil
}

func lspEdgesFromRecords(store graph.Store, records []lspSpoolRecord, scope map[string]struct{}) ([]deferredLSPEdge, []deferredLSPWorkKey) {
	sites := make([]graph.EdgeSite, 0, len(records))
	for _, record := range records {
		sites = append(sites, graph.EdgeSite{From: record.key.from, Line: record.key.line, Kind: record.key.kind})
	}
	candidates := store.GetEdgeCandidates(nil, sites)
	edges := make([]deferredLSPEdge, 0, len(records))
	stale := make([]deferredLSPWorkKey, 0)
	for _, record := range records {
		var live *graph.Edge
		for _, edge := range candidates.Site(record.key.from, record.key.line, record.key.kind) {
			if record.payload.matches(edge) {
				live = edge
				break
			}
		}
		if live == nil {
			stale = append(stale, record.key)
			continue
		}
		if len(scope) > 0 && !edgeInResolveScope(live, scope) {
			continue
		}
		edges = append(edges, deferredLSPEdge{edge: live, target: record.key.target, carried: record.carried})
	}
	return edges, stale
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
