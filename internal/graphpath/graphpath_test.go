package graphpath

import (
	"path/filepath"
	"reflect"
	"testing"
)

// Fixtures build the OS-native rest with filepath.FromSlash, so on POSIX they
// are all-slash (already-correct behavior) and on Windows they reproduce the
// mixed repo/dir\file store shape.

func TestNormJoinsStorePathsWithSlashes(t *testing.T) {
	got := Norm("repo/" + filepath.FromSlash("sub/dir/file.cs"))
	if got != "repo/sub/dir/file.cs" {
		t.Fatalf("Norm = %q, want repo/sub/dir/file.cs", got)
	}
}

func TestDir(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"repo/" + filepath.FromSlash("sub/dir/file.cs"), "repo/sub/dir"},
		{"repo/" + filepath.FromSlash("sub/file.cs"), "repo/sub"},
		{"repo/file.cs", "repo"},
		{"file.cs", ""},
		{"", ""},
	} {
		if got := Dir(tc.in); got != tc.want {
			t.Fatalf("Dir(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestDirDistinguishesSiblingDirectories(t *testing.T) {
	a := Dir("repo/" + filepath.FromSlash("a/x.cs"))
	b := Dir("repo/" + filepath.FromSlash("b/y.cs"))
	if a == b {
		t.Fatalf("sibling directories compare equal: %q", a)
	}
}

func TestPrefixForms(t *testing.T) {
	want := map[string]struct{}{
		"repo/internal/resolver":                          {},
		"repo/" + filepath.FromSlash("internal/resolver"): {},
	}
	got := PrefixForms("repo/internal/resolver")
	gotSet := make(map[string]struct{}, len(got))
	for _, f := range got {
		gotSet[f] = struct{}{}
	}
	if !reflect.DeepEqual(gotSet, want) {
		t.Fatalf("PrefixForms = %v, want keys of %v", got, want)
	}
	if len(got) != len(gotSet) {
		t.Fatalf("PrefixForms returned duplicates: %v", got)
	}
}

func TestPrefixFormsShallow(t *testing.T) {
	for _, in := range []string{"repo", "repo/"} {
		got := PrefixForms(in)
		if len(got) != 1 || got[0] != in {
			t.Fatalf("PrefixForms(%q) = %v, want [%q]", in, got, in)
		}
	}
}

func TestPrefixFormsNormalizesNativeInput(t *testing.T) {
	in := "repo/" + filepath.FromSlash("internal/resolver")
	got := PrefixForms(in)
	found := false
	for _, f := range got {
		if f == "repo/internal/resolver" {
			found = true
		}
	}
	if !found {
		t.Fatalf("PrefixForms(%q) = %v, missing slash form", in, got)
	}
}

func TestHasPrefixMatchesNativeStorePaths(t *testing.T) {
	// The store spelling on Windows; identical to the caller's spelling on
	// POSIX, which is why this case can only fail on the windows runner.
	stored := "repo/" + filepath.FromSlash("internal/resolver/pass.go")
	for _, prefix := range []string{
		"repo/internal/resolver",
		"repo/" + filepath.FromSlash("internal/resolver"),
		"repo/internal",
		"repo",
	} {
		if !HasPrefix(stored, prefix) {
			t.Fatalf("HasPrefix(%q, %q) = false, want true", stored, prefix)
		}
	}
}

func TestHasPrefixRejectsUnrelatedPrefixes(t *testing.T) {
	stored := "repo/" + filepath.FromSlash("internal/resolver/pass.go")
	for _, prefix := range []string{
		"repo/internal/indexer",
		"other/internal/resolver",
	} {
		if HasPrefix(stored, prefix) {
			t.Fatalf("HasPrefix(%q, %q) = true, want false", stored, prefix)
		}
	}
}

func TestHasPrefixEmptyPrefixMatchesEverything(t *testing.T) {
	// Every swept call site previously guarded this with `prefix != ""`.
	if !HasPrefix("repo/"+filepath.FromSlash("a/b.go"), "") {
		t.Fatal(`HasPrefix(path, "") = false, want true`)
	}
}

// The filters this helper replaced were raw string prefixes, not segment
// aware. Pinning that keeps the sweep behaviour-preserving: turning it into a
// segment match would silently change what every analyze path_prefix returns.
func TestHasPrefixStaysARawStringPrefix(t *testing.T) {
	if !HasPrefix("repo/"+filepath.FromSlash("internal/mcpx/tool.go"), "repo/internal/mcp") {
		t.Fatal("HasPrefix stopped matching a partial segment; that is a behaviour change")
	}
}
