package trigram

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestSearcherRefusesSymlinkOutOfRoot is the read-time half of the symlink
// confinement. The indexer walk already keeps an escaping link out of the
// corpus, so this test deliberately hands Build a relative path that IS one
// — modelling a corpus assembled by some other route (a persisted file list
// from an older index, a caller building its own paths). Neither the index
// build nor either scan loop may read through it.
func TestSearcherRefusesSymlinkOutOfRoot(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret.txt")
	const marker = "TRIGRAM-OUT-OF-ROOT-CANARY"
	if err := os.WriteFile(secret, []byte(marker+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package app\n\nfunc Hello() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(root, "pwn.go")); err != nil {
		t.Fatal(err)
	}

	s := Build(root, []string{"main.go", "pwn.go"})

	if got := s.Grep(marker, 0); len(got) != 0 {
		t.Fatalf("Grep read through a symlink out of the root: %+v", got)
	}
	if got := s.GrepRegexp(regexp.MustCompile("OUT-OF-ROOT"), nil, "", 0); len(got) != 0 {
		t.Fatalf("GrepRegexp read through a symlink out of the root: %+v", got)
	}
	// GrepRegexp with no usable literal falls back to scanning every doc,
	// which is the path that bypasses the trigram candidate filter entirely.
	if got := s.GrepRegexp(regexp.MustCompile("."), nil, "", 0); len(got) == 0 {
		t.Fatal("expected the in-root file to still match")
	} else {
		for _, m := range got {
			if m.Path == "pwn.go" {
				t.Fatalf("full scan read through a symlink out of the root: %+v", m)
			}
		}
	}

	// The legitimate file is unaffected.
	if got := s.Grep("func Hello", 0); len(got) != 1 || got[0].Path != "main.go" {
		t.Fatalf("in-root search regressed: %+v", got)
	}
}

// TestSearcherFollowsInRepoSymlink pins that confinement is about leaving
// the root, not about symlinks as such.
func TestSearcherFollowsInRepoSymlink(t *testing.T) {
	root := t.TempDir()
	const marker = "TRIGRAM_IN_ROOT_MARKER"
	if err := os.WriteFile(filepath.Join(root, "shared.go"),
		[]byte("package app\n// "+marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "shared.go"),
		filepath.Join(root, "pkg", "linked.go")); err != nil {
		t.Fatal(err)
	}

	s := Build(root, []string{"shared.go", "pkg/linked.go"})
	got := s.Grep(marker, 0)
	if len(got) != 2 {
		t.Fatalf("an in-root symlink must stay searchable; got %+v", got)
	}
}
