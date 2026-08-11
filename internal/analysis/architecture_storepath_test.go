package analysis

import (
	"path/filepath"
	"testing"
)

// Fixtures build the repo-relative part with filepath.FromSlash, matching the
// store shape (`repo/` + OS-native rest): all-slash on POSIX, mixed
// `repo/dir\file` on Windows.

func TestGlobMatchNativeSeparatorPaths(t *testing.T) {
	p := "r/" + filepath.FromSlash("internal/billing/create.cs")
	if !globMatch("**/billing/**", p) {
		t.Fatalf("glob **/billing/** did not match %q", p)
	}
	if globMatch("**/shipping/**", p) {
		t.Fatalf("glob **/shipping/** matched %q", p)
	}
}

func TestPathHasSegmentNativeSeparatorPaths(t *testing.T) {
	p := "r/" + filepath.FromSlash("internal/domain/user.cs")
	if !pathHasSegment(p, "domain") {
		t.Fatalf("segment domain not found in %q", p)
	}
	if pathHasSegment(p, "user.cs/extra") {
		t.Fatal("non-segment string must not match")
	}
}
