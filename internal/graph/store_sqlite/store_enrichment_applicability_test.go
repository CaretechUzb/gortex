package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// The defect the MINIMUM verdict exists to prevent, pinned at the row level.
//
// With absence meaning "not applicable", a repo where python-types is current
// and go-types has never run looks fully enriched — the fresh provider is the
// only row, so any aggregate over the rows agrees with it. Declaring the
// applicable set first makes the sibling that never started a visible zero, and
// a minimum cannot be fooled by its neighbour.
func TestAnApplicableProviderThatNeverRanIsAVisibleZero(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 1}))

	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types", "python-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "python-types",
		mustContentGen(t, store, "repoA")))

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"go-types": 0, "python-types": 1}, gens)
	require.Zero(t, minProviderGen(gens), "go-types never ran, so the repo is not enriched")
}

// Re-declaring must not undo a completed pass. Applicability is recomputed on
// every enrichment cycle, so an INSERT OR REPLACE here would reset every
// provider to "never ran" on each pass and no repo would ever read ready.
func TestRedeclaringPreservesACompletedProvider(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 1}))

	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "go-types", 1))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types"}))

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(1), gens["go-types"])
}

// The set is authoritative downward too. A repo whose last Python file was
// deleted must stop being judged on python-types — otherwise its gen-0 row
// pegs the repo below ready forever with no action that could ever clear it.
func TestDroppingALanguageDropsItsProviderRow(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types", "python-types"}))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types"}))

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"go-types": 0}, gens)
}

// The whole-repo rollup marker is not a provider and must survive a
// declaration that does not name it — it is what a warm restart reads to decide
// whether to resume at all.
func TestDeclaringLeavesTheRepoRollupMarkerAlone(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	require.NoError(t, store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "repoA", Provider: graph.EnrichProviderRepoMarker, IndexedSHA: "abc",
	}))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types"}))

	st, found, err := store.GetEnrichmentState("repoA", graph.EnrichProviderRepoMarker)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "abc", st.IndexedSHA)
}

// "No provider applies" and "nothing has been recorded" must be different
// answers: the first never blocks ready, the second is an absence of
// information. An empty declared set is the first; no rows at all is the
// second.
func TestTheNoneSentinelDistinguishesNotApplicableFromUnrecorded(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	gens, err := store.EnrichmentContentGens("never-looked-at")
	require.NoError(t, err)
	require.Empty(t, gens)

	require.NoError(t, store.DeclareEnrichmentProviders("repoA", nil))
	gens, err = store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{graph.EnrichProviderNone: 0}, gens)
}

// A repo that later gains its first Go file must stop reading "n/a". Both the
// declaration and a real provider completing clear the sentinel, because the
// two can happen in either order.
func TestTheNoneSentinelClearsWhenARealProviderAppears(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	require.NoError(t, store.DeclareEnrichmentProviders("repoA", nil))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types"}))
	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.NotContains(t, gens, graph.EnrichProviderNone)

	require.NoError(t, store.DeclareEnrichmentProviders("repoB", nil))
	require.NoError(t, store.CompleteEnrichmentProvider("repoB", "go-types", 0))
	gens, err = store.EnrichmentContentGens("repoB")
	require.NoError(t, err)
	require.NotContains(t, gens, graph.EnrichProviderNone)
	require.Contains(t, gens, "go-types")
}

// The caller supplies the content generation here, so the store has to police
// it. A pass cannot claim content that does not exist yet, and a slow pass
// finishing after a fast one cannot walk the row backwards.
func TestCompletionClampsAndNeverRegresses(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 1}))
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 2}))
	require.Equal(t, int64(2), readContentGen(t, store, "repoA"))

	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "go-types", 99))
	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(2), gens["go-types"], "clamped to what actually exists")

	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "go-types", 1))
	gens, err = store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(2), gens["go-types"], "a late slow pass must not regress the row")
}

// SetEnrichmentState names neither readiness column. As INSERT OR REPLACE it
// deleted and reinserted the row, so both silently fell back to DEFAULT 0 — a
// provider that had just recorded a completed pass would read "never ran" the
// instant it recorded its sha.
func TestRecordingTheShaMarkerDoesNotResetTheContentStamp(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 1}))

	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "go-types", 1))
	require.NoError(t, store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "repoA", Provider: "go-types", IndexedSHA: "abc", CompletedAt: 1700, Coverage: 95,
	}))

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(1), gens["go-types"])

	st, found, err := store.GetEnrichmentState("repoA", "go-types")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "abc", st.IndexedSHA)
	require.Equal(t, 95.0, st.Coverage)
}

// The additive-only form. Its caller's "no providers" is a weaker claim than
// the enrichment pass's census, and if it could delete, nothing would restore
// what it removed — the condition that triggers it is the one that stops the
// pass from running again.
func TestTheAdditiveNoneDeclarationNeverDestroys(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)

	require.NoError(t, store.DeclareNoEnrichmentProvidersIfUnrecorded("fresh"))
	gens, err := store.EnrichmentContentGens("fresh")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{graph.EnrichProviderNone: 0}, gens)

	require.NoError(t, store.DeclareEnrichmentProviders("known", []string{"go-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("known", "go-types", 0))
	require.NoError(t, store.DeclareNoEnrichmentProvidersIfUnrecorded("known"))

	gens, err = store.EnrichmentContentGens("known")
	require.NoError(t, err)
	require.Equal(t, map[string]int64{"go-types": 0}, gens)
	require.NotContains(t, gens, graph.EnrichProviderNone)
}

func mustContentGen(t *testing.T, store *Store, prefix string) int64 {
	t.Helper()
	gen, err := store.RepoContentGen(prefix)
	require.NoError(t, err)
	return gen
}

// minProviderGen is the readiness sub-verdict in miniature: the minimum across
// a repo's real provider rows, sentinels excluded.
func minProviderGen(gens map[string]int64) int64 {
	var out int64 = -1
	for provider, gen := range gens {
		if provider == graph.EnrichProviderNone || provider == graph.EnrichProviderRepoMarker {
			continue
		}
		if out < 0 || gen < out {
			out = gen
		}
	}
	if out < 0 {
		return 0
	}
	return out
}

// The upgrade path, and the false alarm it would otherwise deliver to every
// existing install at once.
//
// A store written before the content stamp existed carries enrichment rows with
// an indexed_sha and content_gen 0. Its legacy derive row makes the repo read
// "unknown" until a real derive lands — and the moment it does, the enrichment
// clause becomes live and reports "partial". Permanently: the warm-restart gate
// that would refresh it is the same one deciding the repo needs no pass.
func TestRefreshingCompletedProvidersClearsThePostUpgradeFalseAlarm(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 1}))

	// What the old code left behind: a completed pass with no content stamp.
	require.NoError(t, store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "repoA", Provider: "go-types", IndexedSHA: "abc123", Coverage: 91,
	}))
	require.NoError(t, store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "repoA", Provider: graph.EnrichProviderRepoMarker, IndexedSHA: "abc123",
	}))
	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Zero(t, gens["go-types"], "the pre-upgrade shape: complete, but unstamped")

	n, err := store.RefreshEnrichmentProviders("repoA")
	require.NoError(t, err)
	require.Equal(t, 1, n, "the rollup marker is not a provider and is not counted")

	gens, err = store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(1), gens["go-types"])
	require.Zero(t, gens[graph.EnrichProviderRepoMarker], "sentinels stay out of the verdict")

	st, _, err := store.GetEnrichmentState("repoA", "go-types")
	require.NoError(t, err)
	require.Equal(t, "abc123", st.IndexedSHA, "a refresh renews currency, it does not rewrite provenance")
	require.Equal(t, 91.0, st.Coverage)
}

// The one thing the refresh must not do, and the reason it keys on indexed_sha:
// a provider declared applicable that has never completed a pass has nothing to
// renew, and promoting it would bless the silent subset the whole model exists
// to expose.
func TestRefreshingNeverPromotesAProviderThatNeverRan(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 1}))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types", "python-types"}))
	require.NoError(t, store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "repoA", Provider: "go-types", IndexedSHA: "abc123",
	}))

	n, err := store.RefreshEnrichmentProviders("repoA")
	require.NoError(t, err)
	require.Equal(t, 1, n)

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(1), gens["go-types"])
	require.Zero(t, gens["python-types"], "declared applicable, never ran, still owed")
	require.Zero(t, minProviderGen(gens), "so the repo is still not enriched")
}
