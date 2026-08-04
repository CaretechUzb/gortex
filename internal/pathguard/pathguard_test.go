package pathguard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWithinRoot(t *testing.T) {
	root := filepath.FromSlash("/repo")
	cases := []struct {
		name string
		path string
		root string
		want bool
	}{
		{"the root itself", "/repo", root, true},
		{"a file directly inside", "/repo/main.go", root, true},
		{"a file nested inside", "/repo/pkg/sub/main.go", root, true},
		{"a parent directory", "/", root, false},
		{"a sibling directory", "/other/main.go", root, false},
		{"an escape through ..", "/repo/../etc/passwd", root, false},
		// A sibling whose name merely begins with dots is NOT an escape.
		// A naive strings.HasPrefix(rel, "..") test gets this wrong and
		// would silently drop legitimate files from the index.
		{"a sibling starting with dots", "/repo/..config/app.go", root, true},
		{"a dotted-name sibling of the root", "/..config/app.go", root, false},
		// An empty root confines nothing.
		{"empty root permits anything", "/anywhere/at/all", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := WithinRoot(filepath.FromSlash(tc.path), tc.root)
			if got != tc.want {
				t.Fatalf("WithinRoot(%q, %q) = %v, want %v", tc.path, tc.root, got, tc.want)
			}
		})
	}
}

func TestSymlinkEscapes(t *testing.T) {
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	realFile := filepath.Join(root, "real.go")
	if err := os.WriteFile(realFile, []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}

	escaping := filepath.Join(root, "escaping.go")
	if err := os.Symlink(outsideFile, escaping); err != nil {
		t.Fatal(err)
	}
	internal := filepath.Join(root, "pkg", "internal.go")
	if err := os.Symlink(realFile, internal); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(root, "broken.go")
	if err := os.Symlink(filepath.Join(outside, "does-not-exist"), broken); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"a regular file never escapes", realFile, false},
		{"a link out of the root escapes", escaping, true},
		{"a link within the root does not escape", internal, false},
		{"a broken link is refused", broken, true},
		{"a missing path is left to the caller", filepath.Join(root, "absent.go"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SymlinkEscapes(tc.path, root); got != tc.want {
				t.Fatalf("SymlinkEscapes(%q) = %v, want %v", tc.path, got, tc.want)
			}
			// The pre-resolved variant must agree with the convenience one.
			if got := EscapesResolvedRoot(tc.path, ResolveRoot(root)); got != tc.want {
				t.Fatalf("EscapesResolvedRoot(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// TestSymlinkEscapesUnderSymlinkedRoot covers the macOS /var -> /private/var
// shape: the root itself is reached through a symlink, so a plain in-repo
// file resolves to a path that does not lexically match the unresolved root.
// Without ResolveRoot on the root side, every file in such a repo would look
// like an escape and the index would come up empty.
func TestSymlinkEscapesUnderSymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-root")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(base, "linked-root")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(realRoot, "target.go")
	if err := os.WriteFile(target, []byte("package app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(realRoot, "link.go")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if SymlinkEscapes(filepath.Join(linkedRoot, "link.go"), linkedRoot) {
		t.Fatal("an in-repo link under a symlinked root must not be treated as an escape")
	}
}
