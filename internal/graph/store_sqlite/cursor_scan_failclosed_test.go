package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The cursor scans that materialise a whole result set used to step over a row
// they could not decode. That turns storage corruption into a short slice: the
// caller gets a result that looks complete, and the missing rows read as facts
// the graph does not contain. These three tests pin the fail-closed contract
// on each shape of that family — a full edge scan, a full node scan, and the
// meta-less light projection.

// TestEdgesByKindRaisesOnCorruptEdgeMeta covers the full edge cursor: an
// undecodable meta blob must surface, not silently subtract the edge from
// every "who calls this" answer built on the scan.
func TestEdgesByKindRaisesOnCorruptEdgeMeta(t *testing.T) {
	store := openScanFailStore(t)
	seedScanFixture(t, store)

	if got := drainEdges(store.EdgesByKind(graph.EdgeCalls)); len(got) != 2 {
		t.Fatalf("healthy scan returned %d edges, want 2", len(got))
	}

	corruptColumn(t, store, `UPDATE edges SET meta = ? WHERE from_id = 'pkg/a.go::Alpha'`)

	assertStoreReadRaises(t, "EdgesByKind", func() {
		drainEdges(store.EdgesByKind(graph.EdgeCalls))
	})
}

// TestNodesByKindRaisesOnCorruptNodeMeta is the node-shaped sibling. A node
// scan that quietly drops a row makes the symbol disappear from every census,
// analysis, and enumeration built on it.
func TestNodesByKindRaisesOnCorruptNodeMeta(t *testing.T) {
	store := openScanFailStore(t)
	seedScanFixture(t, store)

	if got := drainNodes(store.NodesByKind(graph.KindFunction)); len(got) != 2 {
		t.Fatalf("healthy scan returned %d nodes, want 2", len(got))
	}

	corruptColumn(t, store, `UPDATE nodes SET meta = ? WHERE id = 'pkg/a.go::Alpha'`)

	assertStoreReadRaises(t, "NodesByKind", func() {
		drainNodes(store.NodesByKind(graph.KindFunction))
	})
}

// TestAllEdgesLightRaisesOnCorruptScalar covers the light projection, which
// never touches the meta blob. Its failure mode is a promoted scalar that will
// not convert — and it must fail the scan for the same reason.
func TestAllEdgesLightRaisesOnCorruptScalar(t *testing.T) {
	store := openScanFailStore(t)
	seedScanFixture(t, store)

	if got := store.AllEdgesLight(); len(got) != 2 {
		t.Fatalf("healthy light scan returned %d edges, want 2", len(got))
	}

	if _, err := store.writerDB.Exec(
		`UPDATE edges SET line = 'not-an-integer' WHERE from_id = 'pkg/a.go::Alpha'`); err != nil {
		t.Fatalf("corrupt line column: %v", err)
	}

	assertStoreReadRaises(t, "AllEdgesLight", func() {
		store.AllEdgesLight()
	})
}

func openScanFailStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "scan-failclosed.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// seedScanFixture writes two functions and two call edges between them, so a
// scan that drops one row still returns the other and looks successful.
func seedScanFixture(t *testing.T, store *Store) {
	t.Helper()
	store.AddBatch(
		[]*graph.Node{
			{ID: "pkg/a.go::Alpha", Kind: graph.KindFunction, Name: "Alpha", FilePath: "pkg/a.go", Language: "go",
				Meta: map[string]any{"note": "alpha"}},
			{ID: "pkg/b.go::Beta", Kind: graph.KindFunction, Name: "Beta", FilePath: "pkg/b.go", Language: "go",
				Meta: map[string]any{"note": "beta"}},
		},
		[]*graph.Edge{
			{From: "pkg/a.go::Alpha", To: "pkg/b.go::Beta", Kind: graph.EdgeCalls, FilePath: "pkg/a.go", Line: 10,
				Meta: map[string]any{"note": "a-to-b"}},
			{From: "pkg/b.go::Beta", To: "pkg/a.go::Alpha", Kind: graph.EdgeCalls, FilePath: "pkg/b.go", Line: 20,
				Meta: map[string]any{"note": "b-to-a"}},
		},
	)
}

// corruptColumn writes bytes no meta codec can read — not the flat binary
// magic, not a JSON object, not gob.
func corruptColumn(t *testing.T, store *Store, stmt string) {
	t.Helper()
	if _, err := store.writerDB.Exec(stmt, []byte("\x7fnot-a-meta-blob")); err != nil {
		t.Fatalf("corrupt meta column: %v", err)
	}
}

func drainEdges(seq func(func(*graph.Edge) bool)) []*graph.Edge {
	var out []*graph.Edge
	for e := range seq {
		out = append(out, e)
	}
	return out
}

func drainNodes(seq func(func(*graph.Node) bool)) []*graph.Node {
	var out []*graph.Node
	for n := range seq {
		out = append(out, n)
	}
	return out
}

// assertStoreReadRaises fails unless read surfaces its storage failure. The
// graph.Store surface returns no error, so panicOnFatal is how a real failure
// reaches the caller; anything quieter is a truncated result the caller reads
// as complete.
func assertStoreReadRaises(t *testing.T, name string, read func()) {
	t.Helper()
	var raised any
	func() {
		defer func() { raised = recover() }()
		read()
	}()
	if raised == nil {
		t.Fatalf("%s returned normally over an undecodable row, want a surfaced storage failure", name)
	}
}
