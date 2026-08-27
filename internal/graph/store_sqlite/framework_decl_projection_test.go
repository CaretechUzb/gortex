package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// The projection exists to decode six columns instead of thirty-five. It is
// only a win if the six it keeps are the six a declaration index reads, and
// Meta is the one that carries the declaration itself — so the round trip
// against the full-node read is what this asserts.
func TestFrameworkDeclProjectionRoundTripsIdentityAndMeta(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "framework-decl.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.AddBatch([]*graph.Node{
		{
			ID: "sale/order.py::SaleOrder", Kind: graph.KindType, Name: "SaleOrder",
			FilePath: "sale/order.py", Language: "python",
			Meta: map[string]any{"odoo_model": "sale.order"},
		},
		{
			ID: "odoo::record::sale.view_order", Kind: graph.KindResource,
			Name: "view_order", FilePath: "sale/views/order.xml", Language: "odoo_xml",
			Meta: map[string]any{"odoo_xml_id": "sale.view_order"},
		},
		// No metadata at all: the NULL blob must not become a non-nil
		// empty map, or "declares nothing" reads as "declares zero things".
		{
			ID: "pkg/foo.go::Bar", Kind: graph.KindType, Name: "Bar",
			FilePath: "pkg/foo.go", Language: "go",
		},
	}, nil)

	full := map[string]*graph.Node{}
	for _, kind := range []graph.NodeKind{graph.KindType, graph.KindResource} {
		for n := range store.NodesByKind(kind) {
			full[n.ID] = n
		}
	}

	seen := map[string]bool{}
	for row := range store.FrameworkDeclNodesSeq(graph.KindType, graph.KindResource) {
		want, ok := full[row.ID]
		if !ok {
			t.Fatalf("projection yielded an unknown node %q", row.ID)
		}
		seen[row.ID] = true
		if row.Kind != want.Kind || row.Name != want.Name ||
			row.FilePath != want.FilePath || row.Language != want.Language {
			t.Fatalf("projection identity diverged for %q: %+v vs %+v", row.ID, row, want)
		}
		if len(row.Meta) != len(want.Meta) {
			t.Fatalf("projection meta diverged for %q: %v vs %v", row.ID, row.Meta, want.Meta)
		}
		for k, v := range want.Meta {
			if row.Meta[k] != v {
				t.Fatalf("projection meta[%q] = %v, want %v", k, row.Meta[k], v)
			}
		}
	}
	if len(seen) != len(full) {
		t.Fatalf("projection yielded %d of %d nodes", len(seen), len(full))
	}
	for row := range store.FrameworkDeclNodesSeq(graph.KindType) {
		if row.ID == "pkg/foo.go::Bar" && row.Meta != nil {
			t.Fatalf("a node with no metadata projected a non-nil map: %#v", row.Meta)
		}
	}
}

// An empty kind set must not degenerate into a scan of the whole table.
func TestFrameworkDeclProjectionNoKindsYieldsNothing(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "framework-decl-empty.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.AddBatch([]*graph.Node{
		{ID: "a", Kind: graph.KindType, Name: "A", FilePath: "a.go", Language: "go"},
	}, nil)

	for row := range store.FrameworkDeclNodesSeq() {
		t.Fatalf("empty kind set yielded %q", row.ID)
	}
}
