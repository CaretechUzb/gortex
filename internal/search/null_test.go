package search

import "testing"

// The null backend must satisfy Backend itself — the optional
// interfaces it must NOT satisfy are asserted below at run time.
var _ Backend = (*NullBackend)(nil)

// TestNullBackend_ImplementsBackendAndNothingElse pins the property the
// fallback seam rests on. The query engine treats a backend as having a
// corpus when Count() is positive OR when it satisfies DocCounter and
// reports a positive disk count; Sizer makes daemon status attribute
// heap to it and ChannelSearcher makes the rerank pipeline ask it for a
// text channel. A NullBackend that grew any of those optional
// interfaces would start claiming a corpus it does not have, and the
// engine would rank against nothing instead of falling back to its
// substring scan.
func TestNullBackend_ImplementsBackendAndNothingElse(t *testing.T) {
	b := NewNull()

	if _, ok := b.(DocCounter); ok {
		t.Error("NullBackend must not implement DocCounter — it would claim a corpus it has no documents for")
	}
	if _, ok := b.(Sizer); ok {
		t.Error("NullBackend must not implement Sizer — it holds no memory to attribute")
	}
	if _, ok := b.(ChannelSearcher); ok {
		t.Error("NullBackend must not implement ChannelSearcher — it has no text channel to contribute")
	}
	if got := BackendSize(b); got != 0 {
		t.Errorf("BackendSize = %d, want 0", got)
	}
}

// TestNullBackend_StaysEmptyAfterAdd is the behaviour the seam promises:
// indexing into the null backend never produces a searchable corpus, so
// callers gated on Count() keep taking their index-free path.
func TestNullBackend_StaysEmptyAfterAdd(t *testing.T) {
	b := NewNull()
	defer b.Close()

	b.Add("pkg/a.go::Alpha", "Alpha", "pkg/a.go", "")
	b.Add("pkg/b.go::Beta", "Beta", "pkg/b.go", "")

	if got := b.Count(); got != 0 {
		t.Errorf("Count after Add = %d, want 0", got)
	}
	if got := b.Search("Alpha", 10); len(got) != 0 {
		t.Errorf("Search returned %d hits, want 0", len(got))
	}

	b.Remove("pkg/a.go::Alpha")
	if got := b.Count(); got != 0 {
		t.Errorf("Count after Remove = %d, want 0", got)
	}
}

// nullIdentityEscapeSink forces both returned pointers to escape. Without it,
// inlining may give two zero-sized values distinct stack addresses and let a
// field-less NullBackend pass the identity assertion accidentally.
var nullIdentityEscapeSink [2]Backend

// TestNewNullReturnsDistinctInstances pins NewNull's identity contract
// directly. NullBackend intentionally carries one byte so independent
// constructions cannot collapse onto runtime.zerobase.
func TestNewNullReturnsDistinctInstances(t *testing.T) {
	nullIdentityEscapeSink = [2]Backend{NewNull(), NewNull()}
	t.Cleanup(func() { nullIdentityEscapeSink = [2]Backend{} })
	if nullIdentityEscapeSink[0] == nullIdentityEscapeSink[1] {
		t.Fatal("NewNull handed back one shared instance")
	}
}
