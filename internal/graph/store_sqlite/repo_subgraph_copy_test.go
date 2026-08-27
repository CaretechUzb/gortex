package store_sqlite

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// copyFixture builds a repository that exercises every id grammar a real one
// carries, including the two that a naive prefix rewrite gets wrong.
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
			FilePath: "models/order.py", Language: "python", RepoPrefix: "local"},
		// Synthetic: the `<prefix>::` grammar. Missed by a `/`-only rewrite.
		{ID: "local::builtin::py::list/sort", Kind: graph.KindFunction, Name: "sort",
			Language: "python", RepoPrefix: "local"},
		{ID: "local::stdlib::argparse::ArgumentParser", Kind: graph.KindFunction,
			Name: "ArgumentParser", Language: "python", RepoPrefix: "local"},
		// A SIBLING CHECKOUT. Starts with "local" but is a different repo.
		{ID: "local@wt/models/order.py::Order", Kind: graph.KindType, Name: "Order",
			FilePath: "models/order.py", Language: "python", RepoPrefix: "local@wt"},
		// Another repository, and a globally-keyed node.
		{ID: "odoo/base/models/res.py::Partner", Kind: graph.KindType, Name: "Partner",
			FilePath: "base/models/res.py", Language: "python", RepoPrefix: "odoo"},
		{ID: "http::GET::/form", Kind: graph.KindContract, Name: "GET /form", RepoPrefix: "local"},
	}, []*graph.Edge{
		{From: "local/models/order.py::Order", To: "local::builtin::py::list/sort", Kind: graph.EdgeCalls},
		{From: "local/models/order.py::Order", To: "odoo/base/models/res.py::Partner", Kind: graph.EdgeExtends},
		{From: "local/models/order.py::Order", To: "http::GET::/form", Kind: graph.EdgeReferences},
		{From: "local/models/order.py::Order", To: "unresolved::odoo::model::sale.order", Kind: graph.EdgeReferences},
		{From: "local@wt/models/order.py::Order", To: "odoo/base/models/res.py::Partner", Kind: graph.EdgeExtends},
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
	// leave the copy's edges pointing back at the source checkout.
	leaked := copyIDs(t, store,
		`SELECT to_id FROM edges WHERE from_repo = 'wt2' AND (substr(to_id,1,6)='local/' OR substr(to_id,1,7)='local::')`)
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
