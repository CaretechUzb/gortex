package store_sqlite

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// copyReadinessFixture builds a source repo that has been indexed, derived and
// enriched, and returns the store.
func copyReadinessFixture(t *testing.T) *Store {
	t.Helper()
	store := openGenTestStore(t)

	store.AddBatch([]*graph.Node{
		{ID: "src/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "src/a.go", RepoPrefix: "src"},
		{ID: "src/b.go::B", Kind: graph.KindFunction, Name: "B", FilePath: "src/b.go", RepoPrefix: "src"},
	}, []*graph.Edge{
		{From: "src/a.go::A", To: "src/b.go::B", Kind: graph.EdgeCalls},
	})
	require.NoError(t, store.BulkSetFileMtimes("src", map[string]int64{"a.go": 100, "b.go": 200}))
	require.NoError(t, store.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: "src", IndexedSHA: "abc123", IndexedAt: 1700,
	}))
	require.NoError(t, store.StampDeriveState([]graph.DeriveCompletion{{
		RepoPrefix: "src", DerivedSHA: "abc123", PassVersion: 1,
	}}, 1700))
	require.NoError(t, store.DeclareEnrichmentProviders("src", []string{"go-types"}))
	require.NoError(t, store.CompleteEnrichmentProvider("src", "go-types",
		mustContentGen(t, store, "src")))
	return store
}

// TestEnrichmentCurrentForRepoSeparatesNeverRanFromFellBehind pins the
// predicate the worktree-copy source ranking asks each candidate.
//
// It is READY's enrichment column, asked about another repo, and it has to
// answer with the same two independent facts: whether any provider has ever
// completed a pass here, and whether the MINIMUM across the real provider rows
// has caught up with the repo's content counter. Collapsing them would make a
// never-enriched source and a source whose pass is unfinished indistinguishable
// — and only the second is the one that laundered a "partial" into a copy's
// "ready".
func TestEnrichmentCurrentForRepoSeparatesNeverRanFromFellBehind(t *testing.T) {
	t.Parallel()
	store := copyReadinessFixture(t)

	current, hasRun, err := store.EnrichmentCurrentForRepo("src")
	require.NoError(t, err)
	require.True(t, hasRun)
	require.True(t, current, "the fixture's provider completed over exactly this content")

	// Content moves; the provider row does not. This is the live shape: a
	// python-types row at content_gen 104 against a repo at 107.
	require.NoError(t, store.ReplaceFileMtimes("src", map[string]int64{"a.go": 555, "b.go": 666}))
	current, hasRun, err = store.EnrichmentCurrentForRepo("src")
	require.NoError(t, err)
	require.True(t, hasRun, "the pass really ran — gen is monotone and nothing walks it back")
	require.False(t, current, "and it ran over content this repo has since moved past")

	// One fresh provider must not speak for a sibling that never started.
	require.NoError(t, store.CompleteEnrichmentProvider("src", "go-types",
		mustContentGen(t, store, "src")))
	require.NoError(t, store.DeclareEnrichmentProviders("src", []string{"go-types", "python-types"}))
	current, hasRun, err = store.EnrichmentCurrentForRepo("src")
	require.NoError(t, err)
	require.True(t, hasRun)
	require.False(t, current,
		"the verdict is the MINIMUM across providers, so a declared-but-never-run "+
			"sibling holds the whole repo back")
}

// The two absences are not the same absence, and neither is vacuously current:
// a repo nobody has enriched must not be read as a repo whose enrichment is
// finished, or the copy ranking would prefer it to a source that really is.
func TestEnrichmentCurrentForRepoIsNotVacuouslyTrueWithoutAProviderThatRan(t *testing.T) {
	t.Parallel()
	store := copyReadinessFixture(t)

	current, hasRun, err := store.EnrichmentCurrentForRepo("never-heard-of-it")
	require.NoError(t, err)
	require.False(t, current)
	require.False(t, hasRun, "no rows at all is nobody having looked")

	require.NoError(t, store.DeclareEnrichmentProviders("declared", []string{"go-types"}))
	current, hasRun, err = store.EnrichmentCurrentForRepo("declared")
	require.NoError(t, err)
	require.False(t, current)
	require.False(t, hasRun,
		"a provider declared applicable and never run is a gen-0 row, which is "+
			"exactly the state that must not be mistaken for completion")

	// Sentinels are rollups, not providers. A repo carrying only __none__ has
	// had nothing enriched, whatever its counters say.
	require.NoError(t, store.DeclareEnrichmentProviders("sentinel", nil))
	require.NoError(t, store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix: "sentinel", Provider: graph.EnrichProviderRepoMarker, IndexedSHA: "abc123",
	}))
	current, hasRun, err = store.EnrichmentCurrentForRepo("sentinel")
	require.NoError(t, err)
	require.False(t, current)
	require.False(t, hasRun)
}

// The exact-copy invariant the re-stamp rests on: a destination that carries
// the same nodes, edges and stage rows as its source is described by those
// carried stamps exactly as well as the source is. If this ever stops holding,
// RestampCopiedReadiness is hiding a real gap rather than clearing a false
// alarm — which is why the invariant is asserted here rather than the re-stamp
// being trusted on its own.
func TestACopyCarriesEveryStageRow(t *testing.T) {
	t.Parallel()
	store := copyReadinessFixture(t)

	res, err := store.CopyRepoSubgraph("src", "dst")
	require.NoError(t, err)
	require.NotZero(t, res.Nodes)

	srcDerive, found, err := store.GetDeriveState("src")
	require.NoError(t, err)
	require.True(t, found)
	dstDerive, found, err := store.GetDeriveState("dst")
	require.NoError(t, err)
	require.True(t, found, "the destination must carry a derive completion, not read never-derived")
	require.Equal(t, srcDerive.PassVersion, dstDerive.PassVersion)
	require.Equal(t, srcDerive.DerivedSHA, dstDerive.DerivedSHA)
	require.False(t, dstDerive.Legacy)

	srcGens, err := store.EnrichmentContentGens("src")
	require.NoError(t, err)
	dstGens, err := store.EnrichmentContentGens("dst")
	require.NoError(t, err)
	require.Equal(t, len(srcGens), len(dstGens), "every provider row travels")
	require.Contains(t, dstGens, "go-types")

	// The counters travel too, so the copy is internally consistent the moment
	// it commits — before anything registers the new checkout.
	_, dstContent, found, err := store.GetRepoGraphGen("dst")
	require.NoError(t, err)
	require.True(t, found)
	require.GreaterOrEqual(t, dstDerive.DerivedContentGen, dstContent)
}

// Registering the copied checkout restats its files, and a worktree's on-disk
// mtimes differ from its source's even at the identical commit. That advances
// the destination's content counter and strands every carried stamp — which is
// what the re-stamp exists to repair, and it must run AFTER that write, not
// inside the copy.
func TestRegisteringACopiedCheckoutStrandsItsStampsUntilRestamped(t *testing.T) {
	t.Parallel()
	store := copyReadinessFixture(t)
	_, err := store.CopyRepoSubgraph("src", "dst")
	require.NoError(t, err)

	// The destination's own restat: same files, different mtimes.
	require.NoError(t, store.ReplaceFileMtimes("dst", map[string]int64{"a.go": 555, "b.go": 666}))

	srcBefore, _, err := store.GetDeriveState("src")
	require.NoError(t, err)

	derive, _, err := store.GetDeriveState("dst")
	require.NoError(t, err)
	_, contentGen, _, err := store.GetRepoGraphGen("dst")
	require.NoError(t, err)
	require.Less(t, derive.DerivedContentGen, contentGen,
		"stranded — this is the permanent false alarm the re-stamp prevents")

	require.NoError(t, store.RestampCopiedReadiness("dst", graph.CopiedReadinessAllStages))

	derive, _, err = store.GetDeriveState("dst")
	require.NoError(t, err)
	require.Equal(t, contentGen, derive.DerivedContentGen)

	gens, err := store.EnrichmentContentGens("dst")
	require.NoError(t, err)
	require.Equal(t, contentGen, gens["go-types"])

	// The source is untouched: a re-stamp names one prefix.
	srcDerive, _, err := store.GetDeriveState("src")
	require.NoError(t, err)
	require.Equal(t, srcBefore.DerivedContentGen, srcDerive.DerivedContentGen)
}

// The one thing the re-stamp must NOT do. A provider declared applicable that
// has never run is a content_gen-0 row, and laundering it into "current"
// because a copy happened would bless the exact silent subset the applicability
// model exists to expose.
func TestRestampingDoesNotLaunderAProviderThatNeverRan(t *testing.T) {
	t.Parallel()
	store := copyReadinessFixture(t)
	require.NoError(t, store.DeclareEnrichmentProviders("src", []string{"go-types", "python-types"}))

	_, err := store.CopyRepoSubgraph("src", "dst")
	require.NoError(t, err)
	require.NoError(t, store.ReplaceFileMtimes("dst", map[string]int64{"a.go": 555}))
	require.NoError(t, store.RestampCopiedReadiness("dst", graph.CopiedReadinessAllStages))

	gens, err := store.EnrichmentContentGens("dst")
	require.NoError(t, err)
	require.Zero(t, gens["python-types"], "declared applicable, never ran, still owed")
	require.NotZero(t, gens["go-types"], "actually ran, and travels as current")
}

// A legacy row asserts nothing about what was derived, so a copy must not
// convert it into a real completion — the destination inherits the same honest
// "unknown" the source reports.
func TestRestampingLeavesALegacyRowLegacy(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{
		{ID: "src/a.go::A", Kind: graph.KindFunction, Name: "A", FilePath: "src/a.go", RepoPrefix: "src"},
	}, nil)
	require.NoError(t, store.BulkSetFileMtimes("src", map[string]int64{"a.go": 100}))
	_, err := store.writerDB.Exec(
		`INSERT INTO derive_state (repo_prefix, legacy) VALUES ('src', 1)`)
	require.NoError(t, err)

	_, err = store.CopyRepoSubgraph("src", "dst")
	require.NoError(t, err)
	require.NoError(t, store.ReplaceFileMtimes("dst", map[string]int64{"a.go": 555}))
	require.NoError(t, store.RestampCopiedReadiness("dst", graph.CopiedReadinessAllStages))

	derive, found, err := store.GetDeriveState("dst")
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, derive.Legacy, "a legacy row stays unknown; a copy is not evidence")
	require.Zero(t, derive.DerivedContentGen)
}

// The premise trackWorktreeByCopy's call ORDER rests on, made executable.
//
// That function used to call RestampCopiedReadiness before ReconcileRepoCtx,
// and the reordering to after it is only correct if a stamp written early is
// genuinely destroyed by the registering write that follows. Until now that
// claim lived in a prose comment, so a later change making the stamp survive
// a subsequent content_gen bump would quietly turn the rationale false and make
// moving the call back look harmless.
//
// The sibling test above proves restamp-AFTER repairs the stranding. This one
// proves restamp-BEFORE does not survive it, which is the half that makes the
// ordering load-bearing rather than incidental.
func TestARestampWrittenBeforeTheRegisteringWriteIsDestroyedByIt(t *testing.T) {
	t.Parallel()
	store := copyReadinessFixture(t)
	_, err := store.CopyRepoSubgraph("src", "dst")
	require.NoError(t, err)

	// The old order: stamp first, while the copy still looks self-consistent.
	require.NoError(t, store.RestampCopiedReadiness("dst", graph.CopiedReadinessAllStages))
	derive, _, err := store.GetDeriveState("dst")
	require.NoError(t, err)
	_, stampedAt, _, err := store.GetRepoGraphGen("dst")
	require.NoError(t, err)
	require.Equal(t, stampedAt, derive.DerivedContentGen, "current at the moment it was written")

	// Then the write the stamp was supposed to cover. ReplaceFileMtimes is what
	// registering the checkout does — the restat, and the eviction of files the
	// source's ledger holds that this checkout lacks.
	require.NoError(t, store.ReplaceFileMtimes("dst", map[string]int64{"a.go": 555}))

	_, contentGen, _, err := store.GetRepoGraphGen("dst")
	require.NoError(t, err)
	require.Greater(t, contentGen, stampedAt, "the registering write advances the counter")

	derive, _, err = store.GetDeriveState("dst")
	require.NoError(t, err)
	require.Less(t, derive.DerivedContentGen, contentGen,
		"stamped before the write it covers, so it is behind again — permanently, "+
			"because an identical copy schedules no derive to re-stamp it")

	gens, err := store.EnrichmentContentGens("dst")
	require.NoError(t, err)
	require.Less(t, gens["go-types"], contentGen, "the enrichment stamp is stranded the same way")
}

// The stage-scoped half of the restamp, proven directly. A diverged copy's
// scoped tail re-derives the changed files, so derive_state is sound to
// declare current for a Derive-only restamp -- but nothing has re-enriched
// anything yet, and the carried enrichment row must NOT be laundered into
// looking current just because a derive happened. This must fail if the
// restamp ever goes back to touching both stages unconditionally.
func TestADivergedCopyRestampsDeriveWithoutBlessingItsCarriedEnrichment(t *testing.T) {
	t.Parallel()
	store := copyReadinessFixture(t)
	_, err := store.CopyRepoSubgraph("src", "dst")
	require.NoError(t, err)

	carried, err := store.EnrichmentContentGens("dst")
	require.NoError(t, err)
	carriedGoTypes := carried["go-types"]

	// The registering write -- same restat
	// TestRegisteringACopiedCheckoutStrandsItsStampsUntilRestamped uses to
	// strand the carried stamps.
	require.NoError(t, store.ReplaceFileMtimes("dst", map[string]int64{"a.go": 555, "b.go": 666}))

	_, dstContentGen, _, err := store.GetRepoGraphGen("dst")
	require.NoError(t, err)

	// The reconcile's scoped tail has re-derived the divergence; only derive
	// is asserted.
	require.NoError(t, store.RestampCopiedReadiness("dst", graph.CopiedReadinessDerive))

	derive, _, err := store.GetDeriveState("dst")
	require.NoError(t, err)
	require.Equal(t, dstContentGen, derive.DerivedContentGen, "derive_state is declared current")

	gens, err := store.EnrichmentContentGens("dst")
	require.NoError(t, err)
	require.Equal(t, carriedGoTypes, gens["go-types"],
		"enrichment_state must be untouched by a Derive-only restamp")
	require.Less(t, gens["go-types"], dstContentGen,
		"the carried enrichment row must NOT read current -- nothing has re-enriched it yet")

	// The scoped enrichment pass that eventually catches this up -- the real
	// mechanism, not the restamp.
	n, err := store.AdvanceContentGenForCompletedProviders("dst", dstContentGen, nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	gens, err = store.EnrichmentContentGens("dst")
	require.NoError(t, err)
	require.Equal(t, dstContentGen, gens["go-types"], "now current, via the scoped pass -- not the restamp")
}

// The Derive-only restamp must never touch enrichment rows at all, including
// a provider that has never run. AllStages already refuses to launder such a
// row via its content_gen > 0 guard; this pins the same refusal for
// Derive-only, where the guard never even needs to fire because enrichment_state
// is not part of the statement list at all.
func TestADeriveOnlyRestampStillRefusesAProviderThatNeverRan(t *testing.T) {
	t.Parallel()
	store := copyReadinessFixture(t)
	require.NoError(t, store.DeclareEnrichmentProviders("src", []string{"go-types", "python-types"}))

	_, err := store.CopyRepoSubgraph("src", "dst")
	require.NoError(t, err)
	require.NoError(t, store.ReplaceFileMtimes("dst", map[string]int64{"a.go": 555}))
	require.NoError(t, store.RestampCopiedReadiness("dst", graph.CopiedReadinessDerive))

	gens, err := store.EnrichmentContentGens("dst")
	require.NoError(t, err)
	require.Zero(t, gens["python-types"], "declared applicable, never ran, still owed")

	_, dstContentGen, _, err := store.GetRepoGraphGen("dst")
	require.NoError(t, err)
	require.Less(t, gens["go-types"], dstContentGen,
		"a Derive-only restamp must not advance enrichment_state at all, even for a provider that DID run")

	derive, _, err := store.GetDeriveState("dst")
	require.NoError(t, err)
	require.Equal(t, dstContentGen, derive.DerivedContentGen, "derive_state IS declared current")
}
