package store_sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// errInjectedFTSFailure stands in for whatever ends a rebuild half-way: a
// failing statement, a disk error, an aborted index.
var errInjectedFTSFailure = errors.New("injected symbol FTS failure")

// TestReplaceSymbolFTSKeepsPriorCorpusWhenRebuildFailsPartway is the
// all-or-nothing contract. Wiping the corpus and then appending chunks commits
// the wipe first, so a failure on a later chunk left the repository with only
// what happened to land before it — a truncated index whose misses read as
// legitimate ones and survive until the next full re-index. A failed
// replacement must leave every document that was searchable before it
// searchable after it.
func TestReplaceSymbolFTSKeepsPriorCorpusWhenRebuildFailsPartway(t *testing.T) {
	store := openFTSReplaceStore(t)

	const repo = "shop"
	const priorCount = 2500
	prior := make([]graph.SymbolFTSItem, 0, priorCount)
	for i := range priorCount {
		prior = append(prior, graph.SymbolFTSItem{
			NodeID: fmt.Sprintf("%s/prior_%04d.go::Prior%04d", repo, i, i),
			Tokens: rareToken("pri", i),
		})
	}
	if err := store.BulkUpsertSymbolFTS(repo, prior); err != nil {
		t.Fatalf("seed prior corpus: %v", err)
	}
	before, err := store.SymbolFTSCount()
	if err != nil {
		t.Fatalf("SymbolFTSCount: %v", err)
	}
	if before != priorCount {
		t.Fatalf("seeded corpus = %d docs, want %d", before, priorCount)
	}

	// The rebuild streams a whole new corpus and dies on the second chunk.
	batches := 0
	err = store.ReplaceSymbolFTS(repo, func(emit func([]graph.SymbolFTSItem) error) error {
		for chunk := range 3 {
			items := make([]graph.SymbolFTSItem, 0, 1024)
			for i := range 1024 {
				n := chunk*1024 + i
				items = append(items, graph.SymbolFTSItem{
					NodeID: fmt.Sprintf("%s/next_%04d.go::Next%04d", repo, n, n),
					Tokens: rareToken("nex", n),
				})
			}
			batches++
			if batches == 2 {
				return errInjectedFTSFailure
			}
			if err := emit(items); err != nil {
				return err
			}
		}
		return nil
	})
	if !errors.Is(err, errInjectedFTSFailure) {
		t.Fatalf("ReplaceSymbolFTS error = %v, want the injected failure", err)
	}

	after, err := store.SymbolFTSCount()
	if err != nil {
		t.Fatalf("SymbolFTSCount after failure: %v", err)
	}
	if after != before {
		t.Fatalf("corpus size after a failed rebuild = %d, want the prior %d", after, before)
	}
	// Size alone would pass on a corpus of the right length but the wrong
	// documents, so check the documents themselves — including the ones past
	// the first chunk boundary, which is where a wipe-then-append rebuild
	// stops having anything to show.
	for _, probe := range []int{0, 1, 1023, 1024, 2048, priorCount - 1} {
		assertSymbolFTSHit(t, store, prior[probe].NodeID, rareToken("pri", probe))
	}
	// Nothing the aborted rebuild emitted may have survived either — a
	// half-published corpus is the same defect wearing the other face.
	assertSymbolFTSMiss(t, store, rareToken("nex", 0))
}

// TestReplaceSymbolFTSPublishesTheWholeCorpusOnSuccess is the other half of
// the contract: a replacement that runs to completion swaps the repository's
// documents wholesale, and leaves a sibling repository's alone.
func TestReplaceSymbolFTSPublishesTheWholeCorpusOnSuccess(t *testing.T) {
	store := openFTSReplaceStore(t)

	if err := store.BulkUpsertSymbolFTS("shop", []graph.SymbolFTSItem{
		{NodeID: "shop/old.go::Retired", Tokens: rareToken("ret", 0)},
	}); err != nil {
		t.Fatalf("seed shop: %v", err)
	}
	if err := store.BulkUpsertSymbolFTS("depot", []graph.SymbolFTSItem{
		{NodeID: "depot/keep.go::Sibling", Tokens: rareToken("sib", 0)},
	}); err != nil {
		t.Fatalf("seed depot: %v", err)
	}

	if err := store.ReplaceSymbolFTS("shop", func(emit func([]graph.SymbolFTSItem) error) error {
		if err := emit([]graph.SymbolFTSItem{
			{NodeID: "shop/new.go::Fresh", Tokens: rareToken("fre", 0)},
		}); err != nil {
			return err
		}
		return emit([]graph.SymbolFTSItem{
			{NodeID: "shop/more.go::Another", Tokens: rareToken("ano", 0)},
		})
	}); err != nil {
		t.Fatalf("ReplaceSymbolFTS: %v", err)
	}

	assertSymbolFTSHit(t, store, "shop/new.go::Fresh", rareToken("fre", 0))
	assertSymbolFTSHit(t, store, "shop/more.go::Another", rareToken("ano", 0))
	assertSymbolFTSHit(t, store, "depot/keep.go::Sibling", rareToken("sib", 0))
	assertSymbolFTSMiss(t, store, rareToken("ret", 0))

	count, err := store.SymbolFTSCount()
	if err != nil {
		t.Fatalf("SymbolFTSCount: %v", err)
	}
	if count != 3 {
		t.Fatalf("corpus = %d docs, want 3 (2 replaced + 1 sibling)", count)
	}
}

func openFTSReplaceStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "fts-replace.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// rareToken builds a fixed-width nonsense word unique to (family, n). The FTS
// query path ORs prefix terms together, so documents that share ordinary words
// all match every ordinary query and a per-document probe has to use a token
// nothing else in the corpus carries.
func rareToken(family string, n int) string {
	letters := [3]byte{}
	for i := 2; i >= 0; i-- {
		letters[i] = byte('a' + n%26)
		n /= 26
	}
	return family + string(letters[:])
}

// assertSymbolFTSHit fails unless query still resolves to wantID through the
// FTS tier. The tier-0 exact-name path needs graph nodes, which these corpora
// deliberately do not have, so every lookup here goes through FTS5.
func assertSymbolFTSHit(t *testing.T, store *Store, wantID, query string) {
	t.Helper()
	hits, err := store.SearchSymbols(query, 20)
	if err != nil {
		t.Fatalf("SearchSymbols(%q): %v", query, err)
	}
	for _, hit := range hits {
		if hit.NodeID == wantID {
			return
		}
	}
	t.Fatalf("document %q is no longer searchable via %q (%d hits)", wantID, query, len(hits))
}

// assertSymbolFTSMiss fails when query matches anything at all.
func assertSymbolFTSMiss(t *testing.T, store *Store, query string) {
	t.Helper()
	hits, err := store.SearchSymbols(query, 20)
	if err != nil {
		t.Fatalf("SearchSymbols(%q): %v", query, err)
	}
	if len(hits) != 0 {
		t.Fatalf("query %q matched %d documents that should not exist: %#v", query, len(hits), hits)
	}
}
