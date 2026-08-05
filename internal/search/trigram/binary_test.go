package trigram

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pngLike returns bytes shaped like a real PNG: a signature carrying NUL
// bytes followed by high-entropy payload. It is the shape that made a
// 2 MiB image cost 101 MiB of trigram index.
func pngLike(payload int) []byte {
	b := make([]byte, 0, payload+8)
	b = append(b, 0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A)
	state := uint32(2463534242)
	for i := 0; i < payload; i++ {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		b = append(b, byte(state))
	}
	return b
}

// mkBinary writes raw bytes at rel, creating parent directories. It is the
// byte-slice counterpart of mk.
func mkBinary(t *testing.T, root, rel string, content []byte) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIsBinary(t *testing.T) {
	cases := []struct {
		name    string
		content []byte
		want    bool
	}{
		{"go source", []byte("package main\n\nfunc main() {}\n"), false},
		{"empty", nil, false},
		{"svg is text", []byte(`<svg width="10" height="10"><rect/></svg>`), false},
		{"utf8 with high bytes", []byte("// héllo wörld — em dash\n"), false},
		{"png signature", pngLike(64), true},
		{"nul at the very end of the window", append(bytes.Repeat([]byte("a"), binarySniffBytes-1), 0), true},
		{"nul past the window is not sniffed", append(bytes.Repeat([]byte("a"), binarySniffBytes+16), 0), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsBinary(tc.content); got != tc.want {
				t.Errorf("IsBinary() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuild_ExcludesBinaryFromIndexAndSearch is the regression for the
// issue #278 finding: a binary asset must contribute nothing to the index
// and must never surface as a match.
func TestBuild_ExcludesBinaryFromIndexAndSearch(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "src/handler.go", "func Handler() { /* marker */ }\n")
	mkBinary(t, root, "assets/graph.png", pngLike(64<<10))

	s := Build(root, []string{"src/handler.go", "assets/graph.png"})

	if got := s.ix.DocCount(); got != 1 {
		t.Errorf("indexed docs = %d, want 1 (the image must not be indexed)", got)
	}
	// The image's docID must not be reachable as a candidate for any query,
	// including the short-query path that returns every known document.
	for _, q := range []string{"marker", "PNG", "ab"} {
		for _, id := range s.candidates(q) {
			if s.paths[id] == "assets/graph.png" {
				t.Fatalf("query %q proposed the binary file as a candidate", q)
			}
		}
	}
	if m := s.Grep("marker", 0); len(m) != 1 || m[0].Path != "src/handler.go" {
		t.Errorf("Grep(marker) = %+v, want the one source hit", m)
	}
}

// TestBuild_KeepsSVGSearchable guards the reason the gate is content-truth
// rather than asset class: the image extractor claims ".svg", but SVG is
// text a user greps.
func TestBuild_KeepsSVGSearchable(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "assets/icon.svg", `<svg viewBox="0 0 1 1"><title>chevron-left</title></svg>`)

	s := Build(root, []string{"assets/icon.svg"})

	if got := s.ix.DocCount(); got != 1 {
		t.Fatalf("indexed docs = %d, want the SVG indexed", got)
	}
	if m := s.Grep("chevron-left", 0); len(m) != 1 {
		t.Errorf("Grep(chevron-left) = %+v, want one hit in the SVG", m)
	}
}

// TestBuild_OversizeIsExcluded pins the per-document sanity ceiling: a
// file over it is never read, and — like an unreadable file — never
// matches. It must NOT become a standing candidate: an earlier draft did
// that, which turned a memory bound into a per-query scan of every large
// generated artifact.
func TestBuild_OversizeIsExcluded(t *testing.T) {
	prev := maxIndexedBytes
	maxIndexedBytes = 1 << 10
	t.Cleanup(func() { maxIndexedBytes = prev })

	root := t.TempDir()
	var big strings.Builder
	for big.Len() < int(maxIndexedBytes)+256 {
		fmt.Fprintf(&big, "// filler line %d aaaaaaaaaaaaaaaaaaaaaaaa\n", big.Len())
	}
	big.WriteString("needleInAHaystack\n")
	mk(t, root, "generated/dump.sql", big.String())
	mk(t, root, "src/small.go", "package src // needleInAHaystack\n")

	s := Build(root, []string{"generated/dump.sql", "src/small.go"})

	if got := s.ix.DocCount(); got != 1 {
		t.Errorf("indexed docs = %d, want only the small file indexed", got)
	}
	if ids := s.ix.docIDs(); len(ids) != 1 || s.paths[ids[0]] != "src/small.go" {
		t.Fatalf("indexed docIDs = %v, want only the small file", ids)
	}
	// Every query path must agree that the oversize file is not a candidate.
	for _, id := range s.candidates("needleInAHaystack") {
		if s.paths[id] == "generated/dump.sql" {
			t.Fatal("oversize file became a standing candidate")
		}
	}
	re := regexp.MustCompile(`needleIn[A-Z]Haystack`)
	for _, m := range s.GrepRegexp(re, nil, "", 0) {
		if m.Path == "generated/dump.sql" {
			t.Fatal("literal-free GrepRegexp scanned the oversize file")
		}
	}
}

// TestGrepRegexp_NoLiteralSkipsBinary covers the fallback branch that scans
// every document when the pattern yields no usable literal.
func TestGrepRegexp_NoLiteralSkipsBinary(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "src/a.go", "package src // aa\n")
	mkBinary(t, root, "bin/blob.dat", pngLike(32<<10))

	s := Build(root, []string{"src/a.go", "bin/blob.dat"})

	re := regexp.MustCompile(`.`)
	for _, m := range s.GrepRegexp(re, nil, "", 0) {
		if m.Path == "bin/blob.dat" {
			t.Fatal("literal-free GrepRegexp scanned the binary document")
		}
	}
}

// TestGrepPathsBounded_SkipsBinary covers the cold, never-builds scan path
// that search_text falls back to before a searcher exists.
func TestGrepPathsBounded_SkipsBinary(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "src/a.go", "const marker = 1\n")
	// Embed the literal inside the binary so a naive scanner would match it.
	mkBinary(t, root, "bin/blob.dat", append(pngLike(1<<10), []byte("marker")...))

	paths := []string{"src/a.go", "bin/blob.dat"}
	matches, stats := GrepPathsBounded(context.Background(), root, paths, "marker", 0, 0)

	if len(matches) != 1 || matches[0].Path != "src/a.go" {
		t.Errorf("matches = %+v, want only the source hit", matches)
	}
	if stats.ScannedFiles != 1 {
		t.Errorf("ScannedFiles = %d, want 1 — the binary must not be scanned", stats.ScannedFiles)
	}

	lit, litStats := GrepLiteralPathsBounded(context.Background(), root, paths, "marker", 0, 0, nil)
	if len(lit) != 1 || lit[0].Path != "src/a.go" {
		t.Errorf("literal matches = %+v, want only the source hit", lit)
	}
	if litStats.ScannedFiles != 1 {
		t.Errorf("literal ScannedFiles = %d, want 1", litStats.ScannedFiles)
	}
}

// TestApproxIndexBytes_TracksPairs pins the budget estimator to the term
// that actually drives index size.
func TestApproxIndexBytes_TracksPairs(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "a.go", "package a\n")
	s := Build(root, []string{"a.go"})

	if got := s.ApproxIndexBytes(); got <= 0 {
		t.Fatalf("ApproxIndexBytes = %d, want positive", got)
	}

	ix := New()
	before := ix.approxBytes()
	ix.Add(0, []byte(strings.Repeat("abcdefghij", 512)))
	after := ix.approxBytes()
	if after <= before {
		t.Errorf("approxBytes did not grow with content: %d -> %d", before, after)
	}
	ix.Remove(0)
	if got := ix.approxBytes(); got != before {
		t.Errorf("approxBytes after Remove = %d, want the empty-index value %d", got, before)
	}
	if ix.pairs != 0 {
		t.Errorf("pairs after Remove = %d, want 0", ix.pairs)
	}
}

// TestSearcher_UpdatePatchesInPlace covers the incremental path: an edit
// must be visible on the next search without rebuilding the corpus, a new
// file must become searchable, and a deleted one must stop matching.
func TestSearcher_UpdatePatchesInPlace(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "src/a.go", "const alpha = 1\n")
	mk(t, root, "src/b.go", "const beta = 2\n")

	s := Build(root, []string{"src/a.go", "src/b.go"})
	if m := s.Grep("alpha", 0); len(m) != 1 {
		t.Fatalf("baseline Grep(alpha) = %+v, want one hit", m)
	}

	// Edited content replaces the old postings.
	mk(t, root, "src/a.go", "const gammaRenamed = 1\n")
	s.Update("src/a.go")
	if m := s.Grep("alpha", 0); len(m) != 0 {
		t.Errorf("Grep(alpha) after edit = %+v, want no hits", m)
	}
	if m := s.Grep("gammaRenamed", 0); len(m) != 1 || m[0].Path != "src/a.go" {
		t.Errorf("Grep(gammaRenamed) = %+v, want the edited file", m)
	}

	// A path the searcher has never seen gets a fresh docID.
	mk(t, root, "src/c.go", "const deltaAdded = 3\n")
	s.Update("src/c.go")
	if m := s.Grep("deltaAdded", 0); len(m) != 1 || m[0].Path != "src/c.go" {
		t.Errorf("Grep(deltaAdded) = %+v, want the new file", m)
	}

	// A deleted file is expressed as an Update over a path that is gone.
	if err := os.Remove(filepath.Join(root, "src", "b.go")); err != nil {
		t.Fatal(err)
	}
	s.Update("src/b.go")
	if m := s.Grep("beta", 0); len(m) != 0 {
		t.Errorf("Grep(beta) after delete = %+v, want no hits", m)
	}
	if got := s.DocCount(); got != 2 {
		t.Errorf("DocCount = %d, want 2 (a.go + c.go)", got)
	}

	// A file that turns binary is dropped rather than indexed.
	mkBinary(t, root, "src/c.go", pngLike(4<<10))
	s.Update("src/c.go")
	if got := s.DocCount(); got != 1 {
		t.Errorf("DocCount after a file turned binary = %d, want 1", got)
	}
}
