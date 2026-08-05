package artifacts

import (
	"os"
	"path/filepath"
	"testing"
)

// TestExpandGlobSkipsSymlinkOutOfRepo covers the symlink-confinement bug
// class in the artifacts manifest. An `artifacts:` entry resolves against
// the repository, so a link out of it must not widen the glob into
// arbitrary host files — get_artifact serves an artifact's bytes verbatim.
//
// All three expansion branches are exercised, because each reaches the
// filesystem a different way (os.Stat, filepath.Glob, filepath.WalkDir) and
// every one of them follows symlinks by default.
func TestExpandGlobSkipsSymlinkOutOfRepo(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "credentials.sql")
	if err := os.WriteFile(secret, []byte("SECRET\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "docs", "real.sql"),
		[]byte("CREATE TABLE t (id int);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(repo, "docs", "leaked.sql")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		pattern string
		wantIn  string
	}{
		{"literal path", "docs/leaked.sql", ""},
		{"single-segment glob", "docs/*.sql", "docs/real.sql"},
		{"recursive glob", "**/*.sql", "docs/real.sql"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := expandGlob(repo, tc.pattern)
			for _, p := range got {
				if p == "docs/leaked.sql" {
					t.Fatalf("expandGlob(%q) admitted a symlink out of the repo: %v", tc.pattern, got)
				}
			}
			if tc.wantIn != "" {
				found := false
				for _, p := range got {
					if p == tc.wantIn {
						found = true
					}
				}
				if !found {
					t.Fatalf("expandGlob(%q) dropped the legitimate file %q: %v", tc.pattern, tc.wantIn, got)
				}
			}
		})
	}
}

// TestExpandGlobKeepsInRepoSymlink pins the boundary: a link that stays
// inside the repository is legitimate and must still expand.
func TestExpandGlobKeepsInRepoSymlink(t *testing.T) {
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, "docs"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repo, "schema.sql")
	if err := os.WriteFile(target, []byte("CREATE TABLE t (id int);\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(repo, "docs", "schema.sql")); err != nil {
		t.Fatal(err)
	}

	got := expandGlob(repo, "docs/*.sql")
	found := false
	for _, p := range got {
		if p == "docs/schema.sql" {
			found = true
		}
	}
	if !found {
		t.Fatalf("an in-repo symlink must still expand; got %v", got)
	}
}
