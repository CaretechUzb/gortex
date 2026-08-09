package search

import "testing"

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
	var b Backend = NewNull()

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

// TestNewNull_DistinctInstances guards the identity contract Swappable
// relies on: Swap closes the previous backend only when it differs from
// the incoming one, so two null backends must not compare equal.
func TestNewNull_DistinctInstances(t *testing.T) {
	if NewNull() == NewNull() {
		t.Error("NewNull must hand back a distinct backend per call")
	}
}
