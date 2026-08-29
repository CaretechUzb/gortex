package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// copyFixture builds a repository that exercises every id grammar a real one
// carries, including the two that a naive prefix rewrite gets wrong.
//
// FilePath is written the way the indexer writes it — prefixed by the repo,
// "local/models/order.py", not the bare relative path. An earlier version of
// this fixture used the bare form, which is why eight green tests never
// noticed that the copy left every path pointing at the source checkout.
func copyFixture(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "copy.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	store.AddBatch([]*graph.Node{
		// File-derived: the `<prefix>/` grammar.
		{ID: "local/models/order.py::Order", Kind: graph.KindType, Name: "Order",
			FilePath: "local/models/order.py", Language: "python", RepoPrefix: "local"},
		// A repository-root file: its generated file_dir is the bare prefix.
		{ID: "local/README.md::doc:readme", Kind: graph.KindDoc, Name: "readme",
			FilePath: "local/README.md", Language: "markdown", RepoPrefix: "local"},
		// Synthetic: the `<prefix>::` grammar. Missed by a `/`-only rewrite.
		{ID: "local::builtin::py::list/sort", Kind: graph.KindFunction, Name: "sort",
			Language: "python", RepoPrefix: "local"},
		{ID: "local::stdlib::argparse::ArgumentParser", Kind: graph.KindFunction,
			Name: "ArgumentParser", Language: "python", RepoPrefix: "local"},
		// A SIBLING CHECKOUT. Starts with "local" but is a different repo.
		{ID: "local@wt/models/order.py::Order", Kind: graph.KindType, Name: "Order",
			FilePath: "local@wt/models/order.py", Language: "python", RepoPrefix: "local@wt"},
		// The sibling's own synthetic node, so the key-range frontier is
		// pinned at both bounds: '@' must sort clear of both '0' and ';'.
		{ID: "local@wt::stdlib::argparse::ArgumentParser", Kind: graph.KindFunction,
			Name: "ArgumentParser", Language: "python", RepoPrefix: "local@wt"},
		// Another repository, and a globally-keyed node.
		{ID: "odoo/base/models/res.py::Partner", Kind: graph.KindType, Name: "Partner",
			FilePath: "odoo/base/models/res.py", Language: "python", RepoPrefix: "odoo"},
		{ID: "http::GET::/form", Kind: graph.KindContract, Name: "GET /form", RepoPrefix: "local"},
	}, []*graph.Edge{
		{From: "local/models/order.py::Order", To: "local::builtin::py::list/sort", Kind: graph.EdgeCalls,
			FilePath: "local/models/order.py"},
		{From: "local/models/order.py::Order", To: "odoo/base/models/res.py::Partner", Kind: graph.EdgeExtends},
		{From: "local/models/order.py::Order", To: "http::GET::/form", Kind: graph.EdgeReferences},
		{From: "local/models/order.py::Order", To: "unresolved::odoo::model::sale.order", Kind: graph.EdgeReferences},
		{From: "local@wt/models/order.py::Order", To: "odoo/base/models/res.py::Partner", Kind: graph.EdgeExtends},
		// Edges SOURCED at a synthetic node. Every other edge here starts at a
		// `local/…` node, which is exactly why eight tests passed while the
		// outbound frontier (`from_repo = ?`) could not see this shape at all.
		{From: "local::stdlib::argparse::ArgumentParser", To: "local::builtin::py::list/sort",
			Kind: graph.EdgeMemberOf},
		{From: "local@wt::stdlib::argparse::ArgumentParser", To: "local@wt/models/order.py::Order",
			Kind: graph.EdgeMemberOf},
	})
	return store
}

func copyIDs(t *testing.T, store *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := store.db.Query(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out = append(out, id)
	}
	return out
}

// The whole point: both prefixed grammars move, and nothing else does.
func TestCopyRepoSubgraph_RewritesBothPrefixedGrammars(t *testing.T) {
	store := copyFixture(t)

	res, err := store.CopyRepoSubgraph("local", "wt2")
	if err != nil {
		t.Fatal(err)
	}
	if res.Nodes == 0 || res.Edges == 0 {
		t.Fatalf("copy moved nothing: %+v", res)
	}

	for _, want := range []string{
		"wt2/models/order.py::Order",
		"wt2::builtin::py::list/sort",
		"wt2::stdlib::argparse::ArgumentParser",
	} {
		got := copyIDs(t, store, `SELECT id FROM nodes WHERE id = ?`, want)
		if len(got) != 1 {
			t.Errorf("expected copied node %q", want)
		}
	}
	// The synthetic grammar is the one a `/`-only rewrite misses, which would
	// leave the copy's edges pointing back at the source checkout. Select the
	// frontier by id range, not by from_repo: from_repo is blind to exactly the
	// synthetic ids this is checking, so it would report "no leak" for an edge
	// it never looked at.
	leaked := copyIDs(t, store,
		`SELECT to_id FROM edges WHERE ((from_id >= 'wt2/' AND from_id < 'wt20') OR (from_id >= 'wt2::' AND from_id < 'wt2;')) `+
			`AND (substr(to_id,1,6)='local/' OR substr(to_id,1,7)='local::')`)
	if len(leaked) != 0 {
		t.Errorf("copy leaks edges back into the source checkout: %v", leaked)
	}
}

// A sibling checkout starts with the source prefix and must not be touched.
// Rewriting on the bare prefix silently merges two checkouts.
func TestCopyRepoSubgraph_LeavesSiblingCheckoutsAlone(t *testing.T) {
	store := copyFixture(t)

	if _, err := store.CopyRepoSubgraph("local", "wt2"); err != nil {
		t.Fatal(err)
	}

	if got := copyIDs(t, store, `SELECT id FROM nodes WHERE id = 'local@wt/models/order.py::Order'`); len(got) != 1 {
		t.Error("the sibling checkout's node was rewritten or removed")
	}
	if got := copyIDs(t, store, `SELECT id FROM nodes WHERE substr(id,1,4) = 'wt2@'`); len(got) != 0 {
		t.Errorf("a sibling checkout was swallowed into the destination: %v", got)
	}
}

// Global and cross-repo targets keep their ids, so the copy reaches the same
// repositories the source does.
func TestCopyRepoSubgraph_PreservesGlobalAndCrossRepoTargets(t *testing.T) {
	store := copyFixture(t)

	if _, err := store.CopyRepoSubgraph("local", "wt2"); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"odoo/base/models/res.py::Partner",
		"http::GET::/form",
		"unresolved::odoo::model::sale.order",
	} {
		got := copyIDs(t, store, `SELECT to_id FROM edges WHERE from_repo = 'wt2' AND to_id = ?`, want)
		if len(got) != 1 {
			t.Errorf("copied repo lost its edge to %q", want)
		}
	}
}

// The copy is additive. A globally-keyed node the source carries already
// exists under its own id, and must be skipped rather than relabelled — that
// is what stops a copy from touching another repository's rows.
func TestCopyRepoSubgraph_NeverRewritesAnExistingGlobalNode(t *testing.T) {
	store := copyFixture(t)

	before := copyIDs(t, store, `SELECT repo_prefix FROM nodes WHERE id = 'http::GET::/form'`)
	if _, err := store.CopyRepoSubgraph("local", "wt2"); err != nil {
		t.Fatal(err)
	}
	after := copyIDs(t, store, `SELECT repo_prefix FROM nodes WHERE id = 'http::GET::/form'`)

	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("global contract node was modified: %v -> %v", before, after)
	}
}

// Prefix-keyed sidecars carry over; they are what a warm restart reads to
// decide the repository is already indexed.
func TestCopyRepoSubgraph_CopiesPrefixKeyedSidecars(t *testing.T) {
	store := copyFixture(t)
	tx, err := store.beginWrite()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES ('local','models/order.py',123)`); err != nil {
		_ = tx.Rollback()
		t.Skipf("file_mtimes not present at this schema: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	res, err2 := store.CopyRepoSubgraph("local", "wt2")
	if err2 != nil {
		t.Fatal(err2)
	}
	if res.Sidecars == 0 {
		t.Error("no sidecar rows copied")
	}
	if got := copyIDs(t, store,
		`SELECT file_path FROM file_mtimes WHERE repo_prefix = 'wt2'`); len(got) != 1 {
		t.Errorf("file_mtimes not carried to the destination: %v", got)
	}
}

func TestCopyRepoSubgraph_RefusesDegenerateArguments(t *testing.T) {
	store := copyFixture(t)
	for _, tc := range [][2]string{{"", "wt2"}, {"local", ""}, {"local", "local"}} {
		if _, err := store.CopyRepoSubgraph(tc[0], tc[1]); err == nil {
			t.Errorf("expected refusal for src=%q dst=%q", tc[0], tc[1])
		}
	}
}

// Edges carry no unique index, so a second copy into the same prefix would
// double every one of them. The destination must be fresh.
func TestCopyRepoSubgraph_RefusesAnOccupiedDestination(t *testing.T) {
	store := copyFixture(t)

	if _, err := store.CopyRepoSubgraph("local", "wt2"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CopyRepoSubgraph("local", "wt2"); err == nil {
		t.Fatal("expected the second copy into an occupied prefix to be refused")
	}
	if _, err := store.CopyRepoSubgraph("local", "odoo"); err == nil {
		t.Fatal("expected a copy into an existing repository to be refused")
	}
}

// A copied repository that is complete in the graph but invisible to
// `search_symbols` is not usable. The FTS corpus carries the source's node
// ids, so it has to be rewritten and its docid map rebuilt.
func TestCopyRepoSubgraph_CopiesTheSearchCorpus(t *testing.T) {
	store := copyFixture(t)
	tx, err := store.beginWrite()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(
		`INSERT INTO symbol_fts (node_id, repo_prefix, tokens) VALUES ('local/models/order.py::Order','local','Order order')`); err != nil {
		_ = tx.Rollback()
		t.Skipf("symbol_fts not present at this schema: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.CopyRepoSubgraph("local", "wt2"); err != nil {
		t.Fatal(err)
	}

	got := copyIDs(t, store, `SELECT node_id FROM symbol_fts WHERE repo_prefix = 'wt2'`)
	if len(got) != 1 || got[0] != "wt2/models/order.py::Order" {
		t.Fatalf("search corpus not carried with rewritten ids: %v", got)
	}
	// The docid map must name the ids SQLite actually assigned, not the
	// source's — a stale map makes every hit resolve to the wrong node.
	mapped := copyIDs(t, store,
		`SELECT r.node_id FROM symbol_fts_rowid r JOIN symbol_fts f ON f.rowid = r.fts_rowid
		  WHERE r.repo_prefix = 'wt2' AND f.node_id = r.node_id`)
	if len(mapped) != 1 {
		t.Fatalf("symbol_fts_rowid does not agree with the copied corpus: %v", mapped)
	}
}

// The regression this file was missing. Eight tests asserted on ids and all
// passed while every copied node kept a file_path pointing at the SOURCE
// checkout — so `get_symbol_source` on the copy resolved to
// "<dst-root>/<src-prefix>/…", a path that does not exist, and every read
// failed. Counting rows cannot see this; only reading the path column can.
func TestCopyRepoSubgraph_RewritesPrefixedPathColumns(t *testing.T) {
	store := copyFixture(t)

	tx, err := store.beginWrite()
	if err != nil {
		t.Fatal(err)
	}
	seed := [][2]string{
		// Prefixed — must be rewritten.
		{`INSERT OR REPLACE INTO files (repo_prefix, file_path) VALUES ('local','local/models/order.py')`, "files"},
		{`INSERT INTO content_fts (node_id, repo_prefix, file_path, ordinal, body) ` +
			`VALUES ('local/README.md::doc:readme','local','local/README.md',0,'hello')`, "content_fts"},
		// Repo-relative — must NOT be rewritten, or the mtime restat breaks.
		{`INSERT OR REPLACE INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES ('local','models/order.py',123)`, "file_mtimes"},
	}
	for _, s := range seed {
		if _, err := tx.Exec(s[0]); err != nil {
			_ = tx.Rollback()
			t.Skipf("%s not present at this schema: %v", s[1], err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	if _, err := store.CopyRepoSubgraph("local", "wt2"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		what  string
		query string
		want  string
	}{
		{"nodes.file_path", `SELECT file_path FROM nodes WHERE id = 'wt2/models/order.py::Order'`, "wt2/models/order.py"},
		// file_dir is VIRTUAL over file_path; a repository-root file gives it
		// the bare prefix, the one value an id rewrite would never produce.
		{"nodes.file_dir", `SELECT file_dir FROM nodes WHERE id = 'wt2/README.md::doc:readme'`, "wt2"},
		{"edges.file_path", `SELECT file_path FROM edges WHERE from_id = 'wt2/models/order.py::Order' AND file_path <> ''`, "wt2/models/order.py"},
		{"files.file_path", `SELECT file_path FROM files WHERE repo_prefix = 'wt2'`, "wt2/models/order.py"},
		{"content_fts.file_path", `SELECT file_path FROM content_fts WHERE repo_prefix = 'wt2'`, "wt2/README.md"},
		{"content_fts_rowid.file_path", `SELECT file_path FROM content_fts_rowid WHERE repo_prefix = 'wt2'`, "wt2/README.md"},
		// The convention trap: this one is repo-relative and stays as it is.
		{"file_mtimes.file_path", `SELECT file_path FROM file_mtimes WHERE repo_prefix = 'wt2'`, "models/order.py"},
	} {
		got := copyIDs(t, store, tc.query)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("%s = %v, want [%s]", tc.what, got, tc.want)
		}
	}

	// Nothing under the source prefix may be left behind in the copy, and the
	// source itself must be untouched.
	if stale := copyIDs(t, store,
		`SELECT file_path FROM nodes WHERE repo_prefix = 'wt2' AND substr(file_path,1,6) = 'local/'`); len(stale) != 0 {
		t.Errorf("copied nodes still point at the source checkout: %v", stale)
	}
	if stale := copyIDs(t, store,
		`SELECT file_path FROM edges WHERE from_repo = 'wt2' AND substr(file_path,1,6) = 'local/'`); len(stale) != 0 {
		t.Errorf("copied edges still point at the source checkout: %v", stale)
	}
	if got := copyIDs(t, store,
		`SELECT file_path FROM nodes WHERE id = 'local/models/order.py::Order'`); len(got) != 1 || got[0] != "local/models/order.py" {
		t.Errorf("source node's path was modified: %v", got)
	}
	// A sibling checkout starts with the source prefix and must not move.
	if got := copyIDs(t, store,
		`SELECT file_path FROM nodes WHERE id = 'local@wt/models/order.py::Order'`); len(got) != 1 || got[0] != "local@wt/models/order.py" {
		t.Errorf("sibling checkout's path was rewritten: %v", got)
	}
}

// A global pass owns an edge by its SOURCE node, so the outbound copy carries
// nothing that another repository points back at the checkout. On the measured
// workspace that is 185,023 edges — a fifth of everything touching it — and
// their absence turns "who uses this" into silence while "what does this
// reference" stays perfect. The asymmetry is invisible to a count of the rows
// the copy did move.
func TestCopyRepoSubgraph_CarriesInboundCrossRepoEdges(t *testing.T) {
	store := copyFixture(t)

	store.AddBatch(nil, []*graph.Edge{
		// Another repository referencing this checkout: must come across, with
		// only to_id moved.
		{From: "odoo/base/models/res.py::Partner", To: "local/models/order.py::Order",
			Kind: graph.EdgeReferences, FilePath: "odoo/base/models/res.py"},
		// A SIBLING checkout referencing it: must not, ever.
		{From: "local@wt/models/order.py::Order", To: "local/models/order.py::Order",
			Kind: graph.EdgeReferences, FilePath: "local@wt/models/order.py"},
	})
	// Without a published grouping the store cannot tell a sibling from a
	// different repository, and the filter below has nothing to filter on.
	store.SetCheckoutGroups(map[string]string{"local": "g1", "local@wt": "g1", "odoo": "g2"})

	res, err := store.CopyRepoSubgraph("local", "wt2")
	if err != nil {
		t.Fatal(err)
	}
	if res.InboundEdges != 1 {
		t.Fatalf("InboundEdges = %d, want 1 (the odoo edge, not the sibling's)", res.InboundEdges)
	}

	got := copyIDs(t, store,
		`SELECT from_id FROM edges WHERE to_id = 'wt2/models/order.py::Order' AND from_repo <> 'wt2'`)
	if len(got) != 1 || got[0] != "odoo/base/models/res.py::Partner" {
		t.Fatalf("inbound edges into the copy = %v, want just the odoo one", got)
	}

	// from_id and file_path name the SOURCE repository's symbol and file. They
	// belong to odoo and must survive the copy untouched.
	if paths := copyIDs(t, store,
		`SELECT file_path FROM edges WHERE to_id = 'wt2/models/order.py::Order' AND from_repo = 'odoo'`); len(paths) != 1 ||
		paths[0] != "odoo/base/models/res.py" {
		t.Errorf("inbound edge's file_path = %v, want the source repository's own path", paths)
	}

	// The sibling's edge is the cross-checkout contamination checkout groups
	// exist to prevent: two checkouts of one repository bound to each other.
	if leaked := copyIDs(t, store,
		`SELECT from_id FROM edges WHERE to_id LIKE 'wt2/%' AND from_repo = 'local@wt'`); len(leaked) != 0 {
		t.Errorf("a sibling checkout's edge was copied into the new checkout: %v", leaked)
	}

	// The source keeps everything it had.
	if orig := copyIDs(t, store,
		`SELECT from_id FROM edges WHERE to_id = 'local/models/order.py::Order' ORDER BY from_id`); len(orig) != 2 {
		t.Errorf("source's inbound edges = %v, want both still present", orig)
	}
}

// The third gap of the same shape. An edge SOURCED at a synthetic `<prefix>::`
// node was never copied, because the outbound frontier selected on from_repo —
// a GENERATED column derived from the first '/' in from_id, which a synthetic
// id either lacks entirely or carries in the wrong place. On the measured
// workspace that silently dropped 245 member_of edges binding stdlib symbols to
// their module, while the derived checkout beside it carried 254.
//
// Nothing already in this file could see it: nodes, paths, FTS rows and inbound
// edges were all at exact parity, and the only count that moved was the edge
// total — which reads as ordinary drift unless you difference it by kind.
func TestCopyRepoSubgraph_CarriesEdgesSourcedAtSyntheticNodes(t *testing.T) {
	store := copyFixture(t)

	if _, err := store.CopyRepoSubgraph("local", "wt2"); err != nil {
		t.Fatal(err)
	}

	got := copyIDs(t, store,
		`SELECT to_id FROM edges WHERE from_id = 'wt2::stdlib::argparse::ArgumentParser'`)
	if len(got) != 1 || got[0] != "wt2::builtin::py::list/sort" {
		t.Fatalf("edge sourced at a synthetic node was not copied: %v", got)
	}

	// The source keeps its own, and the sibling contributes nothing: its
	// synthetic id starts with "local" too, and '@' is what keeps it outside
	// both key ranges.
	if src := copyIDs(t, store,
		`SELECT to_id FROM edges WHERE from_id = 'local::stdlib::argparse::ArgumentParser'`); len(src) != 1 {
		t.Errorf("source edge disturbed: %v", src)
	}
	if dragged := copyIDs(t, store,
		`SELECT from_id FROM edges WHERE to_id LIKE 'wt2%' AND from_id LIKE 'local@wt%'`); len(dragged) != 0 {
		t.Errorf("sibling checkout dragged into the copy: %v", dragged)
	}
}

// An ungrouped store cannot identify a sibling, so it must not be handed one:
// the copy path publishes the grouping first. This pins the store-level half —
// with no grouping the sibling filter is inert, which is exactly why the
// caller's publish is load-bearing rather than decorative.
func TestCopyRepoSubgraph_InboundFilterNeedsAPublishedGrouping(t *testing.T) {
	store := copyFixture(t)
	store.AddBatch(nil, []*graph.Edge{
		{From: "local@wt/models/order.py::Order", To: "local/models/order.py::Order",
			Kind: graph.EdgeReferences},
	})

	if _, err := store.CopyRepoSubgraph("local", "wt2"); err != nil {
		t.Fatal(err)
	}
	leaked := copyIDs(t, store,
		`SELECT from_id FROM edges WHERE to_id LIKE 'wt2/%' AND from_repo = 'local@wt'`)
	if len(leaked) == 0 {
		t.Skip("store now identifies siblings without a published grouping; the caller's publish is no longer load-bearing")
	}
}
