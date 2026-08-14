package store_sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestOpenV6PurgesUnprefixedSoloRepoRows is the upgrade rehearsal for the
// single-repo-mode removal.
//
// A store written before the flip holds a solo repo's file-backed nodes
// under an empty repo_prefix. Nothing in the normal daemon lifecycle can
// reach them — PurgeRepo refuses the empty prefix and OrphanRepoPrefixes
// exempts it — so without this migration the first post-upgrade warmup
// writes a full prefixed copy beside them and the store carries the graph
// twice, with index_health reporting green over it.
//
// The other half of the test is the part that is easy to get wrong: synthetic
// GLOBAL EXTERNALS also carry an empty repo_prefix and must survive. They model
// third-party symbols owned by no repository and visible from every repo, and
// deleting them would make every dep::/external:: target unresolvable
// workspace-wide.
func TestOpenV6PurgesUnprefixedSoloRepoRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		type seedNode struct{ id, filePath, repo string }
		nodes := []seedNode{
			// Solo-repo residue: file-backed, unowned. Must go.
			{"internal/foo.go::Bar", "internal/foo.go", ""},
			{"internal/foo.go", "internal/foo.go", ""},
			{"cmd/main.go::main", "cmd/main.go", ""},
			// Synthetic global externals: no file, no owner. Must stay.
			{"dep::go.uber.org/zap::Logger", "", ""},
			{"external::example.com/shared::Run", "", ""},
			{"module::go:net/http", "", ""},
			// A repo-scoped stub: no file, but owned. Must stay.
			{"repo::builtin::go::make", "", "repo"},
			// A properly prefixed code node. Must stay.
			{"repo/internal/foo.go::Bar", "repo/internal/foo.go", "repo"},
		}
		for _, n := range nodes {
			if _, err := db.Exec(
				`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES (?, 'function', ?, ?, ?)`,
				n.id, n.id, n.filePath, n.repo); err != nil {
				t.Fatalf("seed node %q: %v", n.id, err)
			}
		}

		// An edge wholly inside the solo residue, one crossing from residue
		// into a surviving global, and one wholly between survivors.
		edges := [][2]string{
			{"internal/foo.go::Bar", "cmd/main.go::main"},
			{"internal/foo.go::Bar", "dep::go.uber.org/zap::Logger"},
			{"repo/internal/foo.go::Bar", "dep::go.uber.org/zap::Logger"},
		}
		for _, e := range edges {
			if _, err := db.Exec(
				`INSERT INTO edges (from_id, to_id, kind) VALUES (?, ?, 'calls')`, e[0], e[1]); err != nil {
				t.Fatalf("seed edge %v: %v", e, err)
			}
		}

		// Legacy embeddings have no trustworthy repository or chunk-parent
		// ownership. The later v10 vector-only migration clears this derived
		// table while preserving the graph topology exercised above.
		for _, id := range []string{"internal/foo.go::Bar", "repo/internal/foo.go::Bar"} {
			if _, err := db.Exec(
				`INSERT INTO vectors (node_id, dims, vec) VALUES (?, 1, ?)`, id, []byte{0}); err != nil {
				t.Fatalf("seed vector %q: %v", id, err)
			}
		}

		// Sidecar residue under the empty prefix. file_mtimes especially:
		// leaving it would let the next warm restart skip the re-index that
		// has to happen.
		if _, err := db.Exec(
			`INSERT INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES ('', 'internal/foo.go', 1)`); err != nil {
			t.Fatalf("seed file_mtimes: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO file_mtimes (repo_prefix, file_path, mtime_ns) VALUES ('repo', 'repo/internal/foo.go', 1)`); err != nil {
			t.Fatalf("seed prefixed file_mtimes: %v", err)
		}

		if _, err := db.Exec(`PRAGMA user_version = 6`); err != nil {
			t.Fatalf("reset user_version: %v", err)
		}
	})

	s, err = Open(path)
	if err != nil {
		t.Fatalf("open v6 store: %v", err)
	}
	defer s.Close()

	survivors := queryIDs(t, s.db, `SELECT id FROM nodes ORDER BY id`)
	wantSurvivors := []string{
		"dep::go.uber.org/zap::Logger",
		"external::example.com/shared::Run",
		"module::go:net/http",
		"repo/internal/foo.go::Bar",
		"repo::builtin::go::make",
	}
	assertStringsEqual(t, "surviving nodes", survivors, wantSurvivors)

	// Every edge touching a purged node is gone; the survivor-only edge stays.
	edges := queryIDs(t, s.db, `SELECT from_id || ' -> ' || to_id FROM edges ORDER BY from_id, to_id`)
	assertStringsEqual(t, "surviving edges", edges,
		[]string{"repo/internal/foo.go::Bar -> dep::go.uber.org/zap::Logger"})

	vectors := queryIDs(t, s.db, `SELECT node_id FROM vectors ORDER BY node_id`)
	assertStringsEqual(t, "vectors after v10 derived-cache reset", vectors, nil)

	mtimes := queryIDs(t, s.db, `SELECT repo_prefix || ':' || file_path FROM file_mtimes ORDER BY 1`)
	assertStringsEqual(t, "surviving file_mtimes", mtimes, []string{"repo:repo/internal/foo.go"})

	if v, err := readUserVersion(s.db); err != nil || v != currentSchemaVersion {
		t.Fatalf("user_version = %d (err %v), want %d", v, err, currentSchemaVersion)
	}
}

// TestPurgeUnprefixedRepoRowsIsIdempotent guards the in-place contract:
// every step must be safe to re-run, since a store can reach this migration
// from more than one prior version.
func TestPurgeUnprefixedRepoRowsIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.sqlite")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	withRawDB(t, path, func(db *sql.DB) {
		if _, err := db.Exec(
			`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES ('dep::x::Y', 'function', 'Y', '', '')`); err != nil {
			t.Fatalf("seed global external: %v", err)
		}
		if _, err := db.Exec(
			`INSERT INTO nodes (id, kind, name, file_path, repo_prefix) VALUES ('a.go::A', 'function', 'A', 'a.go', '')`); err != nil {
			t.Fatalf("seed solo residue: %v", err)
		}

		for range 2 {
			tx, err := db.Begin()
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if err := purgeUnprefixedRepoRows(tx); err != nil {
				t.Fatalf("purgeUnprefixedRepoRows: %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}

		assertStringsEqual(t, "nodes after two runs",
			queryIDs(t, db, `SELECT id FROM nodes`), []string{"dep::x::Y"})
	})
}

func queryIDs(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate: %v", err)
	}
	return out
}

func assertStringsEqual(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: got %d (%v), want %d (%v)", label, len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %q, want %q (full: %v)", label, i, got[i], want[i], got)
		}
	}
}
