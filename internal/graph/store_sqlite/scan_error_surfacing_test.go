package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// poisonAfterFirstEdge fails while SQLite is stepping the SECOND edge row:
// zeroblob() past SQLITE_MAX_LENGTH raises at expression-evaluation time, and
// keying the size off the row's own rowid keeps the expression out of the
// planner's constant-hoisting. The scan therefore produces one good row and
// then dies mid-cursor — the exact shape a row-iteration check must catch.
const poisonAfterFirstEdge = ` WHERE length(zeroblob(CASE WHEN id > 1 THEN 3000000000 ELSE 1 END)) >= 0`

// poisonAfterFirstNode is the nodes-table equivalent. nodes is WITHOUT ROWID,
// so the scan runs in primary-key order and the first id sorts first.
const poisonAfterFirstNode = ` WHERE length(zeroblob(CASE WHEN id > 'node::a' THEN 3000000000 ELSE 1 END)) >= 0`

func newScanErrorStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "scan-errors.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch(
		[]*graph.Node{
			{ID: "node::a", Kind: graph.KindFunction, Name: "A", FilePath: "a.go"},
			{ID: "node::b", Kind: graph.KindFunction, Name: "B", FilePath: "b.go"},
			{ID: "node::c", Kind: graph.KindFunction, Name: "C", FilePath: "c.go"},
		},
		[]*graph.Edge{
			{From: "node::a", To: "node::b", Kind: graph.EdgeCalls, FilePath: "a.go", Line: 1},
			{From: "node::b", To: "node::c", Kind: graph.EdgeCalls, FilePath: "b.go", Line: 2},
			{From: "node::c", To: "node::a", Kind: graph.EdgeCalls, FilePath: "c.go", Line: 3},
		},
	)
	return store
}

// recoveredPanic runs fn and reports the value it panicked with, or nil when
// fn returned normally.
func recoveredPanic(fn func()) (recovered any) {
	defer func() { recovered = recover() }()
	fn()
	return nil
}

func assertScanFailureSurfaces(t *testing.T, what string, fn func()) {
	t.Helper()
	recovered := recoveredPanic(fn)
	if recovered == nil {
		t.Fatalf("%s returned normally on a failing scan; a truncated result was reported as success", what)
	}
	err, ok := recovered.(error)
	if !ok {
		t.Fatalf("%s panicked with %T (%v), want an error", what, recovered, recovered)
	}
	if !strings.Contains(err.Error(), "store_sqlite:") {
		t.Fatalf("%s panicked with %v, want a store_sqlite-wrapped storage error", what, err)
	}
}

func prepareOnReader(t *testing.T, store *Store, q string) *sql.Stmt {
	t.Helper()
	stmt, err := store.db.Prepare(q)
	if err != nil {
		t.Fatalf("prepare %q: %v", q, err)
	}
	t.Cleanup(func() { _ = stmt.Close() })
	return stmt
}

// TestEdgeScansSurfaceRowIterationErrors pins the contract that a driver error
// raised part-way through an edge cursor is reported rather than silently
// shortening the returned slice. Both edge scanners take a prepared statement,
// so the failure is injected through the statement's own SQL.
func TestEdgeScansSurfaceRowIterationErrors(t *testing.T) {
	store := newScanErrorStore(t)

	healthy := prepareOnReader(t, store, `SELECT `+lookupEdgeCols+` FROM edges`)
	if got := len(store.queryEdges(healthy)); got != 3 {
		t.Fatalf("healthy edge scan returned %d edges, want 3", got)
	}
	healthyLight := prepareOnReader(t, store, `SELECT `+edgeColsLight+` FROM edges`)
	if got := len(store.queryEdgesLight(healthyLight)); got != 3 {
		t.Fatalf("healthy light edge scan returned %d edges, want 3", got)
	}

	poisoned := prepareOnReader(t, store, `SELECT `+lookupEdgeCols+` FROM edges`+poisonAfterFirstEdge)
	assertScanFailureSurfaces(t, "queryEdges", func() { store.queryEdges(poisoned) })

	poisonedLight := prepareOnReader(t, store, `SELECT `+edgeColsLight+` FROM edges`+poisonAfterFirstEdge)
	assertScanFailureSurfaces(t, "queryEdgesLight", func() { store.queryEdgesLight(poisonedLight) })
}

// TestInlineSQLScansSurfaceQueryAndRowErrors covers the raw-SQL siblings used
// by the predicate iterators: neither the top-level Query error nor a
// mid-cursor driver error may be swallowed into an empty or short result.
func TestInlineSQLScansSurfaceQueryAndRowErrors(t *testing.T) {
	store := newScanErrorStore(t)

	if got := len(store.queryEdgesSQL(`SELECT ` + lookupEdgeCols + ` FROM edges`)); got != 3 {
		t.Fatalf("healthy queryEdgesSQL returned %d edges, want 3", got)
	}
	if got := len(store.queryNodesSQL(`SELECT ` + lookupNodeCols + ` FROM nodes`)); got != 3 {
		t.Fatalf("healthy queryNodesSQL returned %d nodes, want 3", got)
	}

	assertScanFailureSurfaces(t, "queryEdgesSQL on a missing table", func() {
		store.queryEdgesSQL(`SELECT ` + lookupEdgeCols + ` FROM edges_that_do_not_exist`)
	})
	assertScanFailureSurfaces(t, "queryNodesSQL on a missing table", func() {
		store.queryNodesSQL(`SELECT ` + lookupNodeCols + ` FROM nodes_that_do_not_exist`)
	})

	assertScanFailureSurfaces(t, "queryEdgesSQL mid-cursor", func() {
		store.queryEdgesSQL(`SELECT ` + lookupEdgeCols + ` FROM edges` + poisonAfterFirstEdge)
	})
	assertScanFailureSurfaces(t, "queryNodesSQL mid-cursor", func() {
		store.queryNodesSQL(`SELECT ` + lookupNodeCols + ` FROM nodes` + poisonAfterFirstNode)
	})
	assertScanFailureSurfaces(t, "queryEdgesLightSQL mid-cursor", func() {
		store.queryEdgesLightSQL(`SELECT ` + edgeColsLight + ` FROM edges` + poisonAfterFirstEdge)
	})
}

// TestNodeScanHelpersSurfaceRowIterationErrors covers the two node cursors the
// repo-scoped enumerations run on: a scan that dies part-way through must not
// hand back a short slice that reads as the complete set of repo nodes.
func TestNodeScanHelpersSurfaceRowIterationErrors(t *testing.T) {
	store := newScanErrorStore(t)

	if got := len(store.scanNodeQuery(`SELECT ` + lookupNodeCols + ` FROM nodes`)); got != 3 {
		t.Fatalf("healthy scanNodeQuery returned %d nodes, want 3", got)
	}
	if got := len(store.GetRepoNodesLight("")); got != 3 {
		t.Fatalf("healthy GetRepoNodesLight returned %d nodes, want 3", got)
	}

	assertScanFailureSurfaces(t, "scanNodeQuery mid-cursor", func() {
		store.scanNodeQuery(`SELECT ` + lookupNodeCols + ` FROM nodes` + poisonAfterFirstNode)
	})
	assertScanFailureSurfaces(t, "scanNodeQuery on a missing table", func() {
		store.scanNodeQuery(`SELECT ` + lookupNodeCols + ` FROM nodes_that_do_not_exist`)
	})
}
