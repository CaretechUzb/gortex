package merkle

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func write(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func bumpMtime(t *testing.T, abs string) {
	t.Helper()
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(abs, future, future); err != nil {
		t.Fatal(err)
	}
}

func TestBuildAndDiff_DetectsContentChange(t *testing.T) {
	root := t.TempDir()
	write(t, root, "main.go", "package main\n")
	write(t, root, "pkg/util.go", "package pkg\n")
	files := []string{"main.go", "pkg/util.go"}

	t1 := Build(root, files, nil, nil)
	if len(t1.Files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(t1.Files))
	}

	write(t, root, "main.go", "package main\n\nfunc main() {}\n")
	bumpMtime(t, filepath.Join(root, "main.go"))

	t2 := Build(root, files, t1, nil)
	changed, removed := t2.Diff(t1)
	if len(removed) != 0 {
		t.Errorf("no removals expected, got %v", removed)
	}
	if len(changed) != 1 || changed[0] != "main.go" {
		t.Errorf("changed = %v, want [main.go]", changed)
	}
}

func TestDiff_MtimeTouchIsNotAChange(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", "package a\n")
	files := []string{"a.go"}

	t1 := Build(root, files, nil, nil)
	bumpMtime(t, filepath.Join(root, "a.go")) // touch: new mtime, same content

	t2 := Build(root, files, t1, nil)
	changed, _ := t2.Diff(t1)
	if len(changed) != 0 {
		t.Errorf("a touch with unchanged content must not be a change, got %v", changed)
	}
	if t2.Root != t1.Root {
		t.Error("root hash must be stable across a content-preserving touch")
	}
}

func TestDiff_AddAndRemove(t *testing.T) {
	root := t.TempDir()
	write(t, root, "keep.go", "package main\n")
	write(t, root, "gone.go", "package main\n")
	t1 := Build(root, []string{"keep.go", "gone.go"}, nil, nil)

	write(t, root, "new.go", "package main\n")
	t2 := Build(root, []string{"keep.go", "new.go"}, t1, nil)

	changed, removed := t2.Diff(t1)
	if len(changed) != 1 || changed[0] != "new.go" {
		t.Errorf("changed = %v, want [new.go]", changed)
	}
	if len(removed) != 1 || removed[0] != "gone.go" {
		t.Errorf("removed = %v, want [gone.go]", removed)
	}
}

func TestDiff_NilPriorYieldsEverything(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", "x")
	write(t, root, "b.go", "y")
	t1 := Build(root, []string{"a.go", "b.go"}, nil, nil)

	changed, removed := t1.Diff(nil)
	if len(changed) != 2 {
		t.Errorf("nil prior must mark every file changed, got %v", changed)
	}
	if removed != nil {
		t.Errorf("nil prior has no removals, got %v", removed)
	}
}

func TestSubtreeChanged(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/x.go", "package a\n")
	write(t, root, "b/y.go", "package b\n")
	files := []string{"a/x.go", "b/y.go"}
	t1 := Build(root, files, nil, nil)

	write(t, root, "a/x.go", "package a\n\nfunc X() {}\n")
	bumpMtime(t, filepath.Join(root, "a", "x.go"))
	t2 := Build(root, files, t1, nil)

	if !t2.SubtreeChanged("a", t1) {
		t.Error("subtree a changed and must report so")
	}
	if t2.SubtreeChanged("b", t1) {
		t.Error("subtree b is untouched and must report unchanged")
	}
}

func TestSaveLoad_RoundTrip(t *testing.T) {
	root := t.TempDir()
	write(t, root, "main.go", "package main\n")
	t1 := Build(root, []string{"main.go"}, nil, nil)

	path := filepath.Join(t.TempDir(), "nested", "merkle.json")
	if err := t1.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.Root != t1.Root {
		t.Fatalf("loaded root mismatch: %+v", loaded)
	}
	// A tree must diff clean against its own reload.
	changed, _ := t1.Diff(loaded)
	if len(changed) != 0 {
		t.Errorf("a tree must diff clean against its own reload, got %v", changed)
	}
}

func TestLoad_MissingFile(t *testing.T) {
	loaded, err := Load(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatalf("missing file must not error: %v", err)
	}
	if loaded != nil {
		t.Error("missing file must yield a nil tree")
	}
}

func TestDiff_SaltChangeReExtractsOnlyThatLanguage(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", "package a\n")
	write(t, root, "b.py", "x = 1\n")
	files := []string{"a.go", "b.py"}

	// Baseline: the .go file is salted at version 1, .py carries no salt.
	saltV1 := func(rel string) string {
		if filepath.Ext(rel) == ".go" {
			return "go@1"
		}
		return ""
	}
	t1 := Build(root, files, nil, saltV1)

	// Bump only the .go extractor version; file content and mtime are
	// untouched, so the only signal is the salt.
	saltV2 := func(rel string) string {
		if filepath.Ext(rel) == ".go" {
			return "go@2"
		}
		return ""
	}
	t2 := Build(root, files, t1, saltV2)

	changed, removed := t2.Diff(t1)
	if len(removed) != 0 {
		t.Errorf("no removals expected, got %v", removed)
	}
	if len(changed) != 1 || changed[0] != "a.go" {
		t.Errorf("a salt bump must re-flag only the salted language, changed = %v, want [a.go]", changed)
	}
	if t2.Root == t1.Root {
		t.Error("root must move when a file's salt changes")
	}
	// The content hash was reused (no re-read) even though the leaf moved.
	if t2.Files["a.go"].Hash != t1.Files["a.go"].Hash {
		t.Error("content hash must be reused across a salt-only change")
	}
}

func TestBuildWithKnownFiles_UsesParsedSnapshotBeforeDisk(t *testing.T) {
	root := t.TempDir()
	write(t, root, "known.go", "package old")
	knownPath := filepath.Join(root, "known.go")
	info, err := os.Stat(knownPath)
	if err != nil {
		t.Fatal(err)
	}
	knownHash, err := hashFile(knownPath)
	if err != nil {
		t.Fatal(err)
	}
	known := FileNode{Hash: knownHash, Mtime: info.ModTime().UnixNano(), Salt: "stale"}

	// Change the file after the caller captured the bytes it parsed. The tree
	// must describe that parsed snapshot, not the later disk contents.
	write(t, root, "known.go", "package new")
	write(t, root, "fallback.go", "package fallback")
	tree := BuildWithKnownFiles(
		root,
		[]string{"known.go", "fallback.go"},
		nil,
		func(string) string { return "extractor-v2" },
		map[string]FileNode{"known.go": known},
	)

	got := tree.Files["known.go"]
	if got.Hash != knownHash || got.Mtime != known.Mtime {
		t.Fatalf("known node = %+v, want captured hash/mtime %+v", got, known)
	}
	if got.Salt != "extractor-v2" {
		t.Fatalf("known salt = %q, want current extractor salt", got.Salt)
	}
	fallbackHash, err := hashFile(filepath.Join(root, "fallback.go"))
	if err != nil {
		t.Fatal(err)
	}
	if got := tree.Files["fallback.go"].Hash; got != fallbackHash {
		t.Fatalf("fallback hash = %q, want %q", got, fallbackHash)
	}
}

func TestSalt_EmptyEqualsLegacyContentOnly(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a.go", "package a\n")
	files := []string{"a.go"}

	// An empty-salt build, a nil-saltFor build, and the legacy
	// content-only tree must all produce the same root — adopting salts
	// costs nothing until a version is actually bumped.
	emptySalt := func(string) string { return "" }
	withEmpty := Build(root, files, nil, emptySalt)
	withNil := Build(root, files, nil, nil)
	if withEmpty.Root != withNil.Root {
		t.Errorf("empty salt must equal nil saltFor: %s vs %s", withEmpty.Root, withNil.Root)
	}
	if withEmpty.Files["a.go"].Salt != "" {
		t.Errorf("empty salt must be stored empty, got %q", withEmpty.Files["a.go"].Salt)
	}
	changed, _ := withEmpty.Diff(withNil)
	if len(changed) != 0 {
		t.Errorf("empty-salt and nil-salt trees must diff clean, got %v", changed)
	}
}
