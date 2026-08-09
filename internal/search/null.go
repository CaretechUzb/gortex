package search

// NullBackend is the text backend for graph stores that expose no
// native symbol search. It indexes nothing and answers nothing: Add,
// Remove and Close are no-ops, Search returns no hits, and Count
// always reports zero.
//
// Reporting an empty corpus is the point. The query Engine gates its
// ranked path on the backend having something to answer with
// (Engine.backendHasCorpus) and otherwise falls through to its own
// substring scan over the graph, which needs no text index at all.
// NullBackend therefore routes such a store onto exactly the path an
// Engine with no search backend at all already takes in production —
// see the Engine built by pkg/gortex.New, which wires query.NewEngine
// and never calls SetSearch.
//
// NewNull hands back a distinct pointer per call so independent backend
// constructions retain ordinary object identity instead of collapsing onto
// runtime.zerobase.
//
// It deliberately implements Backend and nothing else. Satisfying
// DocCounter would let a disk-corpus count claim a corpus that does
// not exist; Sizer and ChannelSearcher would advertise memory and
// per-channel retrieval it does not have.
type NullBackend struct {
	// A field-less struct has size zero, and the language only
	// promises distinct addresses for variables of non-zero size: every
	// escaping &NullBackend{} would share runtime.zerobase, so any two
	// null backends would compare equal. One byte buys each
	// construction a real address, which is what the identity promise
	// on NewNull rests on. Nothing reads it — the blank name says so.
	_ byte
}

// NewNull returns a Backend that indexes and answers nothing. Every call
// allocates its own instance, so independently constructed null backends have
// distinct identity just like every other backend implementation.
func NewNull() Backend { return &NullBackend{} }

// Add discards the symbol; nothing is indexed.
func (*NullBackend) Add(string, ...string) {}

// Remove is a no-op — nothing was ever indexed.
func (*NullBackend) Remove(string) {}

// Search always returns no hits, so callers fall through to whatever
// index-free path they keep for an empty corpus.
func (*NullBackend) Search(string, int) []SearchResult { return nil }

// Count reports an empty corpus.
func (*NullBackend) Count() int { return 0 }

// Close is a no-op — there are no resources to release.
func (*NullBackend) Close() {}
