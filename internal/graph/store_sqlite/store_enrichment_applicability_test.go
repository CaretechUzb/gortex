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

// TestDeclaringProvidersSparesCheckoutScopedMarkers pins the rule at its
// source. The applicability prune drops every provider row outside the declared
// set, and a checkout marker's key ("<provider>@<checkout>") is never in that
// set — it is not an applicability row, it records that one working copy was
// enriched at one fingerprint.
//
// Without the exclusion, enriching checkout-b deleted checkout-a's marker, so
// every checkout of a family re-enriched a tree the store had already covered
// and the "skip an unchanged working copy" gate never fired for more than the
// most recent one. The two features are independently correct; only their
// composition was not, which is why this is pinned on the store rather than
// left to the end-to-end test in internal/semantic.
func TestDeclaringProvidersSparesCheckoutScopedMarkers(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.go": 1}))

	markerA := "go-types" + graph.EnrichCheckoutMarkerSeparator + "checkout-a"
	require.NoError(t, store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "repoA", Provider: markerA, IndexedSHA: "fingerprint-a",
	}))

	// A declaration naming only the real provider must not disturb the marker.
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types"}))

	got, found, err := store.GetEnrichmentState("repoA", markerA)
	require.NoError(t, err)
	require.True(t, found, "the prune deleted a checkout-scoped marker")
	require.Equal(t, "fingerprint-a", got.IndexedSHA)

	// An unrelated provider row IS still pruned, so the exclusion did not turn
	// the prune off wholesale.
	require.NoError(t, store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "repoA", Provider: "python-types", IndexedSHA: "sha-py",
	}))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types"}))
	_, found, err = store.GetEnrichmentState("repoA", "python-types")
	require.NoError(t, err)
	require.False(t, found, "a provider outside the declared set must still be pruned")
}

// The file-scoped enrichment stamp. A pass over an exact file frontier renews
// the content counter on providers that have already completed, so an
// actively-edited repo stops reading "partial" from its first save onward. The
// tests below pin the four guards that keep the renewal from becoming a lie.

// THE hard correctness hole. gen = 0 is the only signal that re-arms a repo
// whose enrichment has never run, and it is monotone -- once a pass completes,
// the row can never return to zero, so acting on it discharges it for good.
// Advancing a never-ran row's content_gen must leave that zero untouched;
// writing gen (which CompleteEnrichmentProvider does unconditionally, and which
// is why it cannot be used here) would let a one-file pass claim repo-wide
// coverage and the repo would never re-arm again.
func TestAdvancingContentGenLeavesANeverRanProviderAtZero(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	// gen is copied from repo_graph_gen.gen, and that anchor only exists for a
	// prefix that OWNS a node -- so a completed row has gen > 0 only for a repo
	// with real content, which is every repo this path can reach.
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 1}))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"go-types", "python-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "python-types", 1))
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 2}))

	n, err := store.AdvanceContentGenForCompletedProviders("repoA", mustContentGen(t, store, "repoA"), nil)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the completed provider is eligible")

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(2), gens["python-types"], "the completed provider is renewed")
	require.Zero(t, gens["go-types"], "a provider that never ran must not be promoted")

	owed, err := store.EnrichmentNeverRan("repoA")
	require.NoError(t, err)
	require.True(t, owed, "the never-ran re-arm must survive a scoped stamp")
}

// The caller reads the counter before the pass and hands that value back after,
// because enrichment holds no write gate. A value ahead of the live counter is
// therefore always a caller bug, and the clamp turns it into the honest number
// instead of letting the row claim content the store does not hold.
func TestAdvancingContentGenCannotClaimContentThatDoesNotExistYet(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	// gen is copied from repo_graph_gen.gen, and that anchor only exists for a
	// prefix that OWNS a node -- so a completed row has gen > 0 only for a repo
	// with real content, which is every repo this path can reach.
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 1}))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"python-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "python-types", 1))

	_, err := store.AdvanceContentGenForCompletedProviders("repoA", 999, nil)
	require.NoError(t, err)

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, mustContentGen(t, store, "repoA"), gens["python-types"])
	require.Equal(t, int64(1), gens["python-types"], "clamped to what the repo actually holds")
}

// Two passes can overlap, and the slow one finishes last carrying the older
// snapshot. Without the MAX it would walk the row backwards and re-stale a repo
// that a newer pass had already brought current.
func TestAdvancingContentGenNeverMovesARowBackwards(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	// gen is copied from repo_graph_gen.gen, and that anchor only exists for a
	// prefix that OWNS a node -- so a completed row has gen > 0 only for a repo
	// with real content, which is every repo this path can reach.
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 1}))
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 2}))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"python-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "python-types", 2))

	_, err := store.AdvanceContentGenForCompletedProviders("repoA", 1, nil)
	require.NoError(t, err)

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(2), gens["python-types"], "a stale snapshot must not re-stale the row")
}

// Neither sentinel is a provider: __none__ records that the pass looked and
// found nothing applicable, __repo__ is the whole-repo rollup. A scoped pass
// asserts nothing about either, and the rollup in particular must stay
// withheld -- publishing it is what would claim every file was enriched.
func TestAdvancingContentGenLeavesTheSentinelsAlone(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	// gen is copied from repo_graph_gen.gen, and that anchor only exists for a
	// prefix that OWNS a node -- so a completed row has gen > 0 only for a repo
	// with real content, which is every repo this path can reach.
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 1}))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"python-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "python-types", 1))
	// Give both sentinels a non-zero gen, so the exclusion under test is the
	// provider name and not the gen > 0 guard doing the work by accident.
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", graph.EnrichProviderRepoMarker, 1))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", graph.EnrichProviderNone, 1))
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 2}))

	n, err := store.AdvanceContentGenForCompletedProviders("repoA", 2, nil)
	require.NoError(t, err)
	require.Equal(t, 1, n, "only the real provider is eligible")

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(2), gens["python-types"])
	require.Equal(t, int64(1), gens[graph.EnrichProviderRepoMarker], "the rollup is not a provider")
	require.Equal(t, int64(1), gens[graph.EnrichProviderNone], "the sentinel is not a provider")
}

// A provider whose language WAS in the frontier but which produced no result
// had its edges evicted by the re-parse and restored by nothing. Renewing it
// would publish a repo that reads ready while silently missing those edges;
// holding the one row down is the honest reading and keeps the minimum correct.
func TestAdvancingContentGenSkipsExcludedProviders(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	// gen is copied from repo_graph_gen.gen, and that anchor only exists for a
	// prefix that OWNS a node -- so a completed row has gen > 0 only for a repo
	// with real content, which is every repo this path can reach.
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 1}))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"python-types", "ts-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "python-types", 1))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "ts-types", 1))
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 2}))

	n, err := store.AdvanceContentGenForCompletedProviders("repoA", 2, []string{"ts-types"})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(2), gens["python-types"])
	require.Equal(t, int64(1), gens["ts-types"], "an excluded provider must hold the minimum down")
	require.Equal(t, int64(1), minProviderGen(gens), "the repo stays behind, which is correct")
}

// Why this cannot be folded into RefreshEnrichmentProviders. That method filters
// on indexed_sha <> ”, which admits only providers that completed at some
// revision -- and a dirty tree never records one. The repos this stamp exists to
// serve are exactly the ones being edited, so the filter would exclude every
// single one of them.
func TestAdvancingContentGenAdvancesADirtyTreeRowWithNoSha(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	// gen is copied from repo_graph_gen.gen, and that anchor only exists for a
	// prefix that OWNS a node -- so a completed row has gen > 0 only for a repo
	// with real content, which is every repo this path can reach.
	store.AddBatch([]*graph.Node{genNode("repoA", "A")}, nil)
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 1}))
	require.NoError(t, store.DeclareEnrichmentProviders("repoA", []string{"python-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("repoA", "python-types", 1))
	require.NoError(t, store.BulkSetFileMtimes("repoA", map[string]int64{"a.py": 2}))

	refreshed, err := store.RefreshEnrichmentProviders("repoA")
	require.NoError(t, err)
	require.Zero(t, refreshed, "the sha-gated refresh cannot see a dirty-tree row")

	n, err := store.AdvanceContentGenForCompletedProviders("repoA", 2, nil)
	require.NoError(t, err)
	require.Equal(t, 1, n, "the scoped stamp is the one that reaches an edited repo")

	gens, err := store.EnrichmentContentGens("repoA")
	require.NoError(t, err)
	require.Equal(t, int64(2), gens["python-types"])
}
