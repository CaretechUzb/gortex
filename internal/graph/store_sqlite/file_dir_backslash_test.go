package store_sqlite

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// Stored paths keep their native separators: a Windows-written store holds
// `repo/internal\pkg\file.go` — '/' after the repo prefix, '\' below it. The
// generated dir columns must normalize separators before trimming, mirroring
// graphpath.Dir (Norm + trim); otherwise every dir collapses to the repo
// prefix and the receiver-rebind join degenerates from "same package dir"
// to "same repo".
func TestGeneratedDirColumnsNormalizeNativeSeparators(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "dir_cols.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	nodeShapes := []struct {
		filePath string
		wantDir  string
	}{
		{`repo/pkg\types.go`, "repo/pkg"},
		{`repo/pkg/types.go`, "repo/pkg"},
		{`repo/a\b\deep.go`, "repo/a/b"},
		{`solo.go`, "."},
		{`native\solo.go`, "native"},
	}
	for i, sh := range nodeShapes {
		s.AddNode(&graph.Node{
			ID: fmt.Sprintf("n%d", i), Kind: graph.KindType, Name: "T",
			FilePath: sh.filePath, Language: "go", RepoPrefix: "repo",
		})
	}
	for i, sh := range nodeShapes {
		var dir string
		if err := s.db.QueryRow(`SELECT file_dir FROM nodes WHERE id = ?`, fmt.Sprintf("n%d", i)).Scan(&dir); err != nil {
			t.Fatalf("read file_dir for %q: %v", sh.filePath, err)
		}
		if dir != sh.wantDir {
			t.Errorf("file_dir(%q) = %q, want %q", sh.filePath, dir, sh.wantDir)
		}
	}

	edgeShapes := []struct {
		toID    string
		wantDir string
	}{
		{`repo/pkg\types.go::Server`, "repo/pkg"},
		{`repo/pkg/types.go::Server`, "repo/pkg"},
	}
	s.AddNode(&graph.Node{ID: "m", Kind: graph.KindMethod, Name: "M", FilePath: `repo/pkg\m.go`, Language: "go", RepoPrefix: "repo"})
	for i, sh := range edgeShapes {
		s.AddEdge(&graph.Edge{From: "m", To: sh.toID, Kind: graph.EdgeMemberOf, FilePath: `repo/pkg\m.go`, Line: i + 1})
	}
	for _, sh := range edgeShapes {
		var dir sql.NullString
		if err := s.db.QueryRow(`SELECT member_receiver_dir FROM edges WHERE to_id = ?`, sh.toID).Scan(&dir); err != nil {
			t.Fatalf("read member_receiver_dir for %q: %v", sh.toID, err)
		}
		if !dir.Valid || dir.String != sh.wantDir {
			t.Errorf("member_receiver_dir(%q) = %+v, want %q", sh.toID, dir, sh.wantDir)
		}
	}
}

// The rebind join must stay directory-scoped on native-separator paths: a
// same-named type in another directory is not a candidate, and a phantom
// whose only same-named type lives elsewhere stays put.
func TestSQLiteRebindGoMethodReceiversNativeSeparatorPaths(t *testing.T) {
	s := openReceiverRebindStore(t)
	const (
		canonical = `repo/pkg\types.go::Server`
		decoy     = `repo/other\types.go::Server`
		method    = `repo/pkg\methods.go::Server.Run`
		phantom   = `repo/pkg\methods.go::Server`
		strayM    = `repo/x\m.go::Widget.Do`
		strayPh   = `repo/x\m.go::Widget`
		wrongDirT = `repo/y\types.go::Widget`
	)
	s.AddBatch([]*graph.Node{
		{ID: canonical, Kind: graph.KindType, Name: "Server", FilePath: `repo/pkg\types.go`, Language: "go", RepoPrefix: "repo"},
		{ID: decoy, Kind: graph.KindType, Name: "Server", FilePath: `repo/other\types.go`, Language: "go", RepoPrefix: "repo"},
		{ID: method, Kind: graph.KindMethod, Name: "Run", FilePath: `repo/pkg\methods.go`, Language: "go", RepoPrefix: "repo"},
		{ID: strayM, Kind: graph.KindMethod, Name: "Do", FilePath: `repo/x\m.go`, Language: "go", RepoPrefix: "repo"},
		{ID: wrongDirT, Kind: graph.KindType, Name: "Widget", FilePath: `repo/y\types.go`, Language: "go", RepoPrefix: "repo"},
	}, []*graph.Edge{
		{From: method, To: phantom, Kind: graph.EdgeMemberOf, FilePath: `repo/pkg\methods.go`, Line: 1},
		{From: strayM, To: strayPh, Kind: graph.EdgeMemberOf, FilePath: `repo/x\m.go`, Line: 1},
	})

	changed, err := s.RebindGoMethodReceivers("")
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}
	if changed != 1 {
		t.Errorf("changed = %d, want 1 (only the same-directory bind)", changed)
	}
	if receiverEdge(t, s, method, canonical) == nil || receiverEdge(t, s, method, phantom) != nil {
		t.Errorf("native-separator receiver was not rebound to same-dir type %q", canonical)
	}
	if receiverEdge(t, s, method, decoy) != nil {
		t.Errorf("receiver bound to other-directory decoy %q", decoy)
	}
	if receiverEdge(t, s, strayM, strayPh) == nil || receiverEdge(t, s, strayM, wrongDirT) != nil {
		t.Errorf("phantom without a same-directory type was captured by %q", wrongDirT)
	}
}

// The pre-v12 dir expressions, verbatim: they trimmed at '/' only, so
// native-separator paths collapsed the dir to the repo prefix. Kept as a
// fixture so the migration path is exercised against the exact shape a
// stale store carries.
const (
	staleFileDirColumnDDL = `file_dir TEXT GENERATED ALWAYS AS (
    CASE WHEN file_path = '' THEN NULL
         WHEN instr(file_path, '/') = 0 THEN '.'
         WHEN rtrim(rtrim(file_path, replace(file_path, '/', '')), '/') = '' THEN '/'
         ELSE rtrim(rtrim(file_path, replace(file_path, '/', '')), '/') END
) VIRTUAL`

	staleMemberReceiverDirColumnDDL = `member_receiver_dir TEXT GENERATED ALWAYS AS (
    CASE WHEN kind <> 'member_of' OR instr(to_id, '::') <= 1
              OR ` + memberReceiverNameExpr + ` = '' THEN NULL
         WHEN instr(` + memberReceiverFileExpr + `, '/') = 0 THEN '.'
         WHEN rtrim(rtrim(` + memberReceiverFileExpr + `,
                         replace(` + memberReceiverFileExpr + `, '/', '')), '/') = '' THEN '/'
         ELSE rtrim(rtrim(` + memberReceiverFileExpr + `,
                         replace(` + memberReceiverFileExpr + `, '/', '')), '/') END
) VIRTUAL`
)

// A store stamped with the previous schema version carries the stale dir
// expressions; Open must rebuild both generated columns (and the index over
// file_dir) so existing stores heal without a reindex.
func TestSchemaMigrationNormalizesDirColumnSeparators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stale-dirs.sqlite")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw database: %v", err)
	}
	if _, err := raw.Exec(schemaSQL); err != nil {
		t.Fatalf("create base schema: %v", err)
	}
	for _, ddl := range []string{
		`ALTER TABLE nodes ADD COLUMN ` + staleFileDirColumnDDL,
		`ALTER TABLE edges ADD COLUMN ` + staleMemberReceiverDirColumnDDL,
		`CREATE INDEX nodes_go_receiver_type ON nodes(repo_prefix, file_dir, name, id) WHERE language = 'go' AND kind IN ('type', 'interface') AND name <> '' AND file_path <> ''`,
	} {
		if _, err := raw.Exec(ddl); err != nil {
			t.Fatalf("build stale store: %v", err)
		}
	}
	if _, err := raw.Exec(`INSERT INTO nodes (id, kind, name, file_path, language, repo_prefix) VALUES
        ('repo/pkg\types.go::T', 'type', 'T', 'repo/pkg\types.go', 'go', 'repo')`); err != nil {
		t.Fatalf("seed stale node: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO edges (from_id, to_id, kind, file_path, line)
        VALUES ('repo/pkg\m.go::T.M', 'repo/pkg\types.go::T', 'member_of', 'repo/pkg\m.go', 1)`); err != nil {
		t.Fatalf("seed stale edge: %v", err)
	}
	if _, err := raw.Exec(`PRAGMA user_version = 11`); err != nil {
		t.Fatalf("stamp stale version: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw database: %v", err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("open stale store: %v", err)
	}
	var dir string
	if err := s.db.QueryRow(`SELECT file_dir FROM nodes WHERE id = 'repo/pkg\types.go::T'`).Scan(&dir); err != nil {
		t.Fatalf("read migrated file_dir: %v", err)
	}
	if dir != "repo/pkg" {
		t.Errorf("migrated file_dir = %q, want %q", dir, "repo/pkg")
	}
	if err := s.db.QueryRow(`SELECT member_receiver_dir FROM edges WHERE to_id = 'repo/pkg\types.go::T'`).Scan(&dir); err != nil {
		t.Fatalf("read migrated member_receiver_dir: %v", err)
	}
	if dir != "repo/pkg" {
		t.Errorf("migrated member_receiver_dir = %q, want %q", dir, "repo/pkg")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'nodes_go_receiver_type'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("nodes_go_receiver_type after migration count=%d err=%v, want 1,nil", count, err)
	}
	var version int
	if err := s.writerDB.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != currentSchemaVersion {
		t.Fatalf("post-migration user_version=%d err=%v, want %d,nil", version, err, currentSchemaVersion)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close migrated store: %v", err)
	}

	// A second open finds the current shape and must not error or re-alter.
	s, err = Open(path)
	if err != nil {
		t.Fatalf("second migrated open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second migrated close: %v", err)
	}
}
