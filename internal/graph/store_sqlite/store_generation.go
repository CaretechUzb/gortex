package store_sqlite

// baseViewGeneration is the generation every store starts on: the single
// corpus written by a plain index. The catalog mints new generations from 1
// upwards, so 0 always names the base payload rows.
const baseViewGeneration int64 = 0

// AtGeneration returns a handle over the same database pinned to payload view
// generation g. The returned handle shares the core — pools, prepared
// statements, caches, write gate — with the receiver, so it sees every write
// any other handle makes; only the pinned generation differs.
//
// The derived handle never owns the core: closing it is a no-op, and the
// handle Open returned stays responsible for teardown.
//
// A negative generation is a programming error and returns nil rather than
// silently falling back to the base corpus.
func (s *Store) AtGeneration(g int64) *Store {
	if g < baseViewGeneration {
		return nil
	}
	return &Store{storeCore: s.storeCore, viewGen: g}
}

// ViewGeneration reports the payload view generation this handle is pinned to.
// The handle Open returned reads the base corpus, generation 0.
func (s *Store) ViewGeneration() int64 { return s.viewGen }
