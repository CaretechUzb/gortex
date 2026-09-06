package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/readiness"
	"github.com/zzet/gortex/internal/semantic"
)

// copySourcePrefix is the prefix copyTrackHarness tracks its source repository
// under. Named here because every test below seeds enrichment rows on the
// SOURCE and then reads them back through the destination's copy.
const copySourcePrefix = "src"

// harnessStore reaches the sqlite store behind copyTrackHarness.
//
// The copy path only exists on a backend that implements CopyRepoSubgraph, so
// this assertion cannot fail without the harness having silently stopped
// testing the thing it is for.
func harnessStore(t *testing.T, mi *MultiIndexer) *store_sqlite.Store {
	t.Helper()
	store, ok := mi.graph.(*store_sqlite.Store)
	require.True(t, ok, "the copy-track harness must be sqlite-backed")
	require.NotNil(t, mi.GetIndexer(copySourcePrefix),
		"the harness must have tracked the source under %q", copySourcePrefix)
	return store
}

// seedCompletedProvider gives the source repository one provider row that has
// completed a pass over its current content — the shape a real copy source is
// in, and the only shape in which the destination's carried rows can be read as
// "behind" rather than "never looked at".
func seedCompletedProvider(t *testing.T, store *store_sqlite.Store, prefix, provider string) {
	t.Helper()
	require.NoError(t, store.DeclareEnrichmentProviders(prefix, []string{provider}))
	contentGen, err := store.RepoContentGen(prefix)
	require.NoError(t, err)
	require.NoError(t, store.CompleteEnrichmentProvider(prefix, provider, contentGen))
}

// seedCompletedDerive gives the source repository a derived-pass row over its
// current content, so the copy carries one.
//
// The harness deliberately tracks its source inside a batch and clears the
// deferred rederive, which leaves the source genuinely never-derived — a state
// readiness reports ahead of every enrichment clause. Seeding the row is what
// lets the verdict fall through to the stage this test is about.
func seedCompletedDerive(t *testing.T, store *store_sqlite.Store, prefix string) {
	t.Helper()
	require.NoError(t, store.StampDeriveState(
		[]graph.DeriveCompletion{{RepoPrefix: prefix}}, time.Now().Unix()))
}

// seedLaggingProvider gives the source a provider row that HAS completed a pass
// — gen is non-zero, so nothing reads it as never-run — over content the source
// has since moved past.
//
// This is the live shape, not a synthetic one: the sibling that was chosen as a
// copy source on 2026-09-05 held a python-types row at content_gen 104 against
// its repo's 107, because that pass had never finished. content_gen is left
// above zero deliberately — the enrichment re-stamp skips rows still at zero,
// so a row at zero would be laundering-proof for the wrong reason and the test
// would pass without the fix.
func seedLaggingProvider(t *testing.T, store *store_sqlite.Store, prefix, provider string) {
	t.Helper()
	seedCompletedProvider(t, store, prefix, provider)
	bumpRepoContent(t, store, prefix)

	gens, err := store.EnrichmentContentGens(prefix)
	require.NoError(t, err)
	require.Positive(t, gens[provider],
		"a row at content_gen 0 is skipped by the re-stamp anyway; this one must be one it would move")
	current, hasRun, err := store.EnrichmentCurrentForRepo(prefix)
	require.NoError(t, err)
	require.True(t, hasRun, "the row must read as a pass that RAN, not one that never did")
	require.False(t, current, "and as one whose content has moved on since")
}

// bumpRepoContent advances a repo's content counter without changing its graph
// — the shape a re-stat leaves behind, and the one that separates a provider
// row that is current from one that is merely finished.
//
// The mtime VALUES are irrelevant to everything downstream: the copy path
// re-stats the source's ledger from disk and keeps only its key set, so
// perturbing them moves the counter and nothing else.
func bumpRepoContent(t *testing.T, store *store_sqlite.Store, prefix string) {
	t.Helper()
	before, err := store.RepoContentGen(prefix)
	require.NoError(t, err)
	mtimes := store.LoadFileMtimes(prefix)
	require.NotEmpty(t, mtimes, "the source must have an mtime ledger to perturb")
	for path := range mtimes {
		mtimes[path]++
	}
	require.NoError(t, store.ReplaceFileMtimes(prefix, mtimes))
	after, err := store.RepoContentGen(prefix)
	require.NoError(t, err)
	require.Greater(t, after, before, "a re-stat must move the content counter")
}

// trackIdenticalCopy tracks a worktree sitting on exactly the commit the
// source's rows describe: the copy path's identical branch, where there is no
// divergence to reconcile and no scoped tail to re-derive anything.
func trackIdenticalCopy(t *testing.T, mi *MultiIndexer, wt string, logs *observer.ObservedLogs) *IndexResult {
	t.Helper()
	res, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: wt, AsWorktree: true})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotEmpty(t, res.RepoPrefix)
	require.Len(t, logs.FilterMessage("worktree installed by subgraph copy").All(), 1,
		"the track must have taken the copy path, not a cold index")
	require.Empty(t, logs.FilterMessage(
		"worktree copy: divergence repaired by the reconcile's scoped tail; no workspace rederive owed").All(),
		"precondition: an identical copy has no divergence to repair")
	return res
}

// sourceIndexedSHA is the commit the source's SUBGRAPH describes — what
// copySourceCommit returns, and therefore the commit a copy is taken AT.
func sourceIndexedSHA(t *testing.T, store *store_sqlite.Store, prefix string) string {
	t.Helper()
	st, found, err := store.GetRepoIndexState(prefix)
	require.NoError(t, err)
	require.True(t, found, "the tracked source must have an index-state row")
	require.False(t, st.Dirty, "copySourceCommit refuses a dirty source outright")
	require.NotEmpty(t, st.IndexedSHA)
	return st.IndexedSHA
}

// writeRepoMarker plants the whole-repo enrichment completion marker the copy
// will carry over verbatim.
func writeRepoMarker(t *testing.T, store *store_sqlite.Store, prefix, sha string) {
	t.Helper()
	require.NoError(t, store.SetEnrichmentState(graph.EnrichmentState{
		RepoPrefix:  prefix,
		Provider:    graph.EnrichProviderRepoMarker,
		IndexedSHA:  sha,
		CompletedAt: time.Now().Unix(),
	}))
}

// peekCopiedEnrichMarkerPromotion reads the entitlement WITHOUT consuming it —
// takeCopiedEnrichMarkerPromotion is one-shot by design, so an assertion that
// used it could not be followed by a second one.
func peekCopiedEnrichMarkerPromotion(idx *Indexer) string {
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	return idx.copiedEnrichMarkerSHA
}

// destinationReadinessInputs reads the destination's readiness exactly the way
// `gortex repos` does: one open of the store, the raw rows, and the pure
// verdict over them. PassVersion / ConfigHash are left zero, which skips the
// two clauses that compare against a running daemon there is none of here.
func destinationReadinessInputs(t *testing.T, store *store_sqlite.Store, prefix string) readiness.Inputs {
	t.Helper()
	states, err := store.ReadinessStates()
	require.NoError(t, err)
	repo, ok := states.Repos[prefix]
	require.True(t, ok, "the copied checkout must appear in the readiness rows")
	return readiness.Inputs{
		DeriveTable: states.DeriveTable,
		EnrichTable: states.EnrichTable,
		Repo:        repo,
	}
}

// TestADivergedCopyDoesNotReadReadyUntilItsScopedEnrichmentCompletes is the
// end-to-end gate on the READY lie.
//
// A worktree installed by a diverged subgraph copy used to have BOTH its stage
// stamps declared current the moment the reconcile's scoped tail returned —
// derive_state truthfully, enrichment_state falsely, because nothing had
// re-enriched anything. `gortex repos` then read `ready` over a repository
// whose semantic pass had not started, and kept reading it for the two-odd
// minutes the pass took. The stamp split is what makes the interval honest:
// enrichment is brought forward by the scoped pass itself
// (CompleteScopedEnrichment), never by the restamp.
//
// The harness has no semantic Manager, so the armed pass never dispatches —
// which is precisely the window under test.
func TestADivergedCopyDoesNotReadReadyUntilItsScopedEnrichmentCompletes(t *testing.T) {
	mi, repo, logs := copyTrackHarness(t)
	store := harnessStore(t, mi)
	seedCompletedProvider(t, store, copySourcePrefix, "go-types")
	seedCompletedDerive(t, store, copySourcePrefix)

	wt := divergedWorktree(t, repo, "readiness")
	res := trackDivergedCopy(t, mi, wt, logs)

	in := destinationReadinessInputs(t, store, res.RepoPrefix)
	require.Positive(t, in.Repo.EnrichProviders,
		"precondition: the copy must have carried the source's provider row")
	require.True(t, in.Repo.DeriveFound,
		"precondition: the copy must have carried the source's derive row")
	require.GreaterOrEqual(t, in.Repo.Derive.DerivedContentGen, in.Repo.ContentGen,
		"precondition: the derive stamp IS restamped current — derivation really "+
			"did cover this checkout, and it must not be what withholds ready")

	require.Equal(t, readiness.EnrichLabelStale, readiness.EnrichVerdict(in),
		"the carried enrichment row describes the SOURCE's content; nothing has "+
			"re-enriched this checkout, so enrichment is behind its content")

	label, reason := readiness.Verdict(
		readiness.RepoState{Indexed: true, Path: wt}, in)
	require.NotEqual(t, readiness.LabelReady, label,
		"a copy whose scoped enrichment has not run must not read ready")
	require.Equal(t, readiness.LabelPartial, label, reason)
}

// TestACopyFromALaggingSourceOwesAWholeRepoPass is the gate on the laundering
// this change exists to stop.
//
// Measured live 2026-09-05. A worktree tracked at `local`~2 chose a sibling copy
// at `local`~1 — closer, and honestly `partial`: its python-types row sat at
// content_gen 104 against the repo's 107 because that pass had never finished.
// The copy carried the lagging row verbatim; its one-file scoped repair then
// completed, and CompleteScopedEnrichment ->
// AdvanceContentGenForCompletedProviders advanced EVERY gen > 0 row — the
// inherited one included — to the copy's own counter. READY read `ready` over a
// graph whose python enrichment had been finished nowhere.
//
// A scoped frontier cannot be the answer here, whatever its size: the rows are
// behind on files this checkout did not change, so no frontier drawn from the
// divergence covers them. The whole repository is the only honest frontier, and
// the pass over it writes its own completion marker — so there is nothing for
// the scoped promotion to carry either.
func TestACopyFromALaggingSourceOwesAWholeRepoPass(t *testing.T) {
	t.Run("a diverged copy arms the whole repository, not a scoped repair", func(t *testing.T) {
		mi, repo, logs := copyTrackHarness(t)
		store := harnessStore(t, mi)
		seedLaggingProvider(t, store, copySourcePrefix, "go-types")
		seedCompletedDerive(t, store, copySourcePrefix)
		// Evidence that WOULD entitle a scoped promotion, so the refusal is on
		// the source's currency and not on a missing marker.
		writeRepoMarker(t, store, copySourcePrefix, sourceIndexedSHA(t, store, copySourcePrefix))

		wt := divergedWorktree(t, repo, "lagging")
		res := trackDivergedCopy(t, mi, wt, logs)
		require.Len(t, logs.FilterMessage(
			"worktree copy: divergence repaired by the reconcile's scoped tail; no workspace rederive owed").All(), 1,
			"precondition: the derivation half really was repaired inline — this is "+
				"the branch that used to arm the scoped repair")

		idx := mi.GetIndexer(res.RepoPrefix)
		require.NotNil(t, idx)
		full, pending, repair := deferredEnrichArm(idx)
		require.True(t, pending)
		require.True(t, full,
			"the carried rows describe a corpus the SOURCE had not finished; no "+
				"frontier drawn from this divergence can complete them")
		require.False(t, repair,
			"a whole-repo pass already runs under the repo-scaled deadline and "+
				"already escalates nothing further")
		require.Empty(t, peekCopiedEnrichMarkerPromotion(idx),
			"EnrichAll writes the marker itself; a second writer is one too many")
		behind := logs.FilterMessage(
			"worktree copy: source enrichment is behind; copy owes a whole-repo pass").All()
		require.Len(t, behind, 1, "the extra minutes must have a visible cause in the log")
		require.Equal(t, true, behind[0].ContextMap()["source_enrichment_ever_ran"],
			"and the line must separate a source nothing has enriched from one whose "+
				"pass ran and fell behind — this fixture is the second")

		in := destinationReadinessInputs(t, store, res.RepoPrefix)
		require.Equal(t, readiness.EnrichLabelStale, readiness.EnrichVerdict(in),
			"and the copy must read behind until that pass actually runs")
	})

	t.Run("an identical copy arms one too, and blesses only its derive", func(t *testing.T) {
		mi, repo, logs := copyTrackHarness(t)
		store := harnessStore(t, mi)
		seedLaggingProvider(t, store, copySourcePrefix, "go-types")
		seedCompletedDerive(t, store, copySourcePrefix)

		// Same commit as the source's rows: the branch that used to restamp
		// BOTH stages and arm nothing at all.
		wt := addWorktree(t, repo, "identical")
		res := trackIdenticalCopy(t, mi, wt, logs)

		idx := mi.GetIndexer(res.RepoPrefix)
		require.NotNil(t, idx)
		full, pending, repair := deferredEnrichArm(idx)
		require.True(t, pending,
			"an identical copy of an unfinished enrichment inherits the debt whole; "+
				"nothing else in the system will notice it before the next restart")
		require.True(t, full)
		require.False(t, repair)
		require.Empty(t, peekCopiedEnrichMarkerPromotion(idx))
		require.Len(t, logs.FilterMessage(
			"worktree copy: source enrichment is behind; copy owes a whole-repo pass").All(), 1)

		in := destinationReadinessInputs(t, store, res.RepoPrefix)
		require.True(t, in.Repo.DeriveFound)
		require.GreaterOrEqual(t, in.Repo.Derive.DerivedContentGen, in.Repo.ContentGen,
			"derive is still asserted: the carried derived edges describe this "+
				"commit exactly, which is why the copy was allowed at all")
		require.Equal(t, readiness.EnrichLabelStale, readiness.EnrichVerdict(in),
			"enrichment is not: restamping it would declare the source's unfinished "+
				"pass complete for a checkout it never covered")
	})
}

// TestACopyFromACurrentSourceStillArmsTheScopedRepair is the other side of the
// same gate. Currency is a precondition on the scoped repair, not a new reason
// to distrust the copy: a source that finished its own enrichment hands over
// rows that are exact everywhere outside the divergence, and re-enriching the
// whole repository to learn a handful of files is the cost this path exists to
// avoid.
func TestACopyFromACurrentSourceStillArmsTheScopedRepair(t *testing.T) {
	mi, repo, logs := copyTrackHarness(t)
	store := harnessStore(t, mi)
	seedCompletedProvider(t, store, copySourcePrefix, "go-types")
	seedCompletedDerive(t, store, copySourcePrefix)

	current, hasRun, err := store.EnrichmentCurrentForRepo(copySourcePrefix)
	require.NoError(t, err)
	require.True(t, hasRun)
	require.True(t, current, "precondition: the source finished its own pass")

	wt := divergedWorktree(t, repo, "currentsource")
	res := trackDivergedCopy(t, mi, wt, logs)

	idx := mi.GetIndexer(res.RepoPrefix)
	require.NotNil(t, idx)
	full, pending, repair := deferredEnrichArm(idx)
	require.True(t, pending)
	require.False(t, full,
		"a current source's carried rows are exact outside the divergence, so the "+
			"frontier really is the divergence")
	require.True(t, repair,
		"and the dispatch must still be told it is a repair rather than a save")
	idx.deferredEnrichMu.Lock()
	files := len(idx.deferredEnrichFiles)
	idx.deferredEnrichMu.Unlock()
	require.Positive(t, files)
	require.Empty(t, logs.FilterMessage(
		"worktree copy: source enrichment is behind; copy owes a whole-repo pass").All())
}

// TestAScopedCopyRepairArmsMarkerPromotionOnlyWhenTheInheritedMarkerNamesTheCopiedCommit
// pins the evidence check that makes the promotion sound.
//
// The inherited __repo__ row records the source's last COMPLETE enrichment,
// which is not the same thing as the commit the subgraph was copied at: the
// source may have been enriched, then re-indexed at a newer commit. Only when
// the two coincide does the source's completion describe the corpus this
// checkout carries verbatim, and only then may a scoped repair of the
// divergence finish the whole-repo assertion.
func TestAScopedCopyRepairArmsMarkerPromotionOnlyWhenTheInheritedMarkerNamesTheCopiedCommit(t *testing.T) {
	const foreignSHA = "0123456789abcdef0123456789abcdef01234567"

	cases := []struct {
		name          string
		marker        func(srcSHA string) (sha string, write bool)
		wantPromotion bool
	}{
		{
			name:          "the marker names the copied commit",
			marker:        func(srcSHA string) (string, bool) { return srcSHA, true },
			wantPromotion: true,
		},
		{
			name:          "the marker names some other revision",
			marker:        func(string) (string, bool) { return foreignSHA, true },
			wantPromotion: false,
		},
		{
			name:          "there is no marker at all",
			marker:        func(string) (string, bool) { return "", false },
			wantPromotion: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mi, repo, logs := copyTrackHarness(t)
			store := harnessStore(t, mi)
			seedCompletedProvider(t, store, copySourcePrefix, "go-types")
			if sha, write := tc.marker(sourceIndexedSHA(t, store, copySourcePrefix)); write {
				writeRepoMarker(t, store, copySourcePrefix, sha)
			}

			wt := divergedWorktree(t, repo, "promotion")
			res := trackDivergedCopy(t, mi, wt, logs)
			idx := mi.GetIndexer(res.RepoPrefix)
			require.NotNil(t, idx, "the copied checkout must have an indexer")

			// Precondition: the arm really was the SCOPED one. A whole-repo arm
			// would make every assertion below pass for the wrong reason.
			idx.deferredEnrichMu.Lock()
			scoped := !idx.deferredEnrichFull && len(idx.deferredEnrichFiles) > 0
			idx.deferredEnrichMu.Unlock()
			require.True(t, scoped,
				"an inline diverged copy arms a file-scoped repair over the reconciled frontier")

			got := peekCopiedEnrichMarkerPromotion(idx)
			if tc.wantPromotion {
				require.Equal(t, gitHeadSHA(wt), got,
					"the promotion is armed at THIS checkout's HEAD, the revision the "+
						"marker would be rewritten to")
				require.Len(t, logs.FilterMessage(
					"worktree copy: inherited enrichment marker names the copied commit; scoped repair may promote it").All(), 1)
			} else {
				require.Empty(t, got,
					"a marker that does not name the copied commit proves nothing about "+
						"the carried corpus, so no promotion may be armed")
				require.Len(t, logs.FilterMessage(
					"worktree copy: inherited enrichment marker does not name the copied commit; no promotion armed").All(), 1,
					"a declined promotion must be visible in the log, not silent")
			}
		})
	}
}

// TestTheInheritedMarkerIsPromotedOnlyByANonPartialNonWithholdingCompletion
// covers the second half of the conjunction: the entitlement is necessary, and
// on its own not sufficient.
//
// The predicate is extracted so the four ways the two proofs stop composing can
// be enumerated without running a provider, and the write it guards is checked
// against a real Manager on a real store so "allowed" and "the marker moved"
// cannot drift apart.
func TestTheInheritedMarkerIsPromotedOnlyByANonPartialNonWithholdingCompletion(t *testing.T) {
	const (
		armed = "3ed9c0864742f0b1c9a15f1e6f2f1f9d2b3c4d5e"
		moved = "0123456789abcdef0123456789abcdef01234567"
	)

	cases := []struct {
		name     string
		want     string
		sha      string
		dirty    bool
		withheld []string
		allowed  bool
	}{
		{
			name: "a clean completion still at the armed revision promotes",
			want: armed, sha: armed, allowed: true,
		},
		{
			name: "nothing armed promotes nothing",
			want: "", sha: armed, allowed: false,
		},
		{
			name: "HEAD moved under the pass",
			want: armed, sha: moved, allowed: false,
		},
		{
			name: "a dirty tree is not the committed state a sha names",
			want: armed, sha: armed, dirty: true, allowed: false,
		},
		{
			name: "a withheld provider is a hole in the frontier",
			want: armed, sha: armed, withheld: []string{"python-types"}, allowed: false,
		},
		{
			name: "an empty sha is not a revision",
			want: armed, sha: "", allowed: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed := copiedMarkerPromotionAllowed(tc.want, tc.sha, tc.dirty, tc.withheld)
			require.Equal(t, tc.allowed, allowed)

			// The guarded write, spelled exactly as runDeferredEnrich spells it.
			store := openTestSqlite(t)
			mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
			if allowed {
				mgr.RecordRepoEnrichmentComplete(store, "r", tc.sha, tc.dirty)
			}

			marker, found, err := store.GetEnrichmentState("r", graph.EnrichProviderRepoMarker)
			require.NoError(t, err)
			if tc.allowed {
				require.True(t, found, "a promoted marker must be persisted")
				require.Equal(t, tc.sha, marker.IndexedSHA)
			} else {
				require.False(t, found,
					"a declined promotion must leave the marker exactly as the copy "+
						"inherited it, so the next restart re-arms the full pass")
			}
		})
	}

	// The guard is load-bearing rather than decorative: RecordRepoEnrichmentComplete
	// itself has no opinion about which revision a scoped frontier covered, so
	// called unguarded on a moved HEAD it writes a marker naming content this
	// pass never saw — and the next restart would then skip the pass that is
	// genuinely owed.
	t.Run("unguarded, the same call blesses a revision the pass never covered", func(t *testing.T) {
		store := openTestSqlite(t)
		mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
		mgr.RecordRepoEnrichmentComplete(store, "r", moved, false)

		marker, found, err := store.GetEnrichmentState("r", graph.EnrichProviderRepoMarker)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, moved, marker.IndexedSHA)
	})

	// One entitlement, one pass. A later scoped pass over some other frontier —
	// a watcher save — carries none of the evidence that made this one sound.
	t.Run("the entitlement is one-shot", func(t *testing.T) {
		idx := &Indexer{}
		idx.armCopiedEnrichMarkerPromotion(armed)
		require.Equal(t, armed, idx.takeCopiedEnrichMarkerPromotion())
		require.Empty(t, idx.takeCopiedEnrichMarkerPromotion(),
			"a second pass must not inherit the first one's proof")

		idx.armCopiedEnrichMarkerPromotion("")
		require.Empty(t, idx.takeCopiedEnrichMarkerPromotion(),
			"an empty revision is not an entitlement")
	})
}

// newCopiedPromotionFixture is one sqlite-backed indexer over a REAL git
// checkout, armed with a file-scoped frontier and left on a clean tree.
//
// The git repository is the part the other scoped-pass fixtures do not need:
// the promotion is guarded on the pass's own (sha, dirty) reading, so a
// non-checkout root would make every guard vacuously false and the test prove
// nothing. The frontier edit is COMMITTED for the same reason — it is the shape
// a copy's reconcile leaves behind, and the marker names a committed state.
func newCopiedPromotionFixture(t *testing.T) (*Indexer, *store_sqlite.Store, string) {
	t.Helper()
	idx, store, repo := newScopedEnrichFixture(t, &spyEnrichProvider{}, zap.NewNop())
	return idx, store, repo
}

// newScopedEnrichFixture is newCopiedPromotionFixture with the provider and the
// logger supplied by the caller, so a test can choose what the scoped pass
// REPORTS (partial, failed, clean) and read the lines it logged about it.
func newScopedEnrichFixture(
	t *testing.T, provider semantic.Provider, logger *zap.Logger,
) (*Indexer, *store_sqlite.Store, string) {
	t.Helper()
	repo := commitGitRepo(t)
	store := openTestSqlite(t)

	idx := New(store, newTestRegistry(), config.IndexConfig{}, logger)
	idx.SetRepoPrefix("r")
	idx.SetRootPath(repo)
	// The derived passes are irrelevant here and cost seconds each.
	idx.deferGlobalPasses.Store(true)

	idx.SetSemanticManager(func() *semantic.Manager {
		mgr := semantic.NewManager(semantic.Config{
			Enabled: true,
			// A small, non-zero save timeout so a repair's deadline is bound by
			// CopyRepairDeadline's cap rather than coinciding with the uncapped
			// repo scaling — see the "dispatched repair runs under the
			// repo-scaled deadline" subtest below, which depends on the two
			// diverging. None of these fixtures' providers actually wait out a
			// real deadline (they return synchronously), so this has no effect
			// on their timing.
			TimeoutSeconds: 10,
			Providers: []semantic.ProviderConfig{{
				Name: provider.Name(), Languages: provider.Languages(), Priority: 1, Enabled: true,
			}},
		}, zap.NewNop())
		mgr.RegisterProvider(provider)
		return mgr
	}())

	main := filepath.Join(repo, "main.go")
	require.NoError(t, idx.IndexFile(main))
	info, err := os.Stat(main)
	require.NoError(t, err)
	idx.SetFileMtimes(map[string]int64{"main.go": info.ModTime().UnixNano()})

	require.NoError(t, os.WriteFile(main,
		[]byte("package main\n\nfunc main() { _ = 1 }\n"), 0o644))
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "the divergence")
	future := time.Now().Add(time.Minute)
	require.NoError(t, os.Chtimes(main, future, future))

	res, err := idx.IncrementalReindexPaths(repo, []string{main})
	require.NoError(t, err)
	require.Equal(t, 1, res.StaleFileCount)
	require.Empty(t, res.FailedFiles)

	files, full, _ := idx.deferredEnrichScope()
	require.False(t, full, "the fixture must arm a file-scoped pass, not a repo one")
	require.Len(t, files, 1)
	return idx, store, repo
}

// TestAScopedPassWritesTheWholeRepoMarkerOnlyForTheRevisionItWasArmedAt drives
// the promotion through runDeferredEnrich itself.
//
// The predicate is checked as a unit above; this is the wiring around it — that
// a completed scoped pass reaches RecordRepoEnrichmentComplete at all, and that
// it stays away from the marker when the entitlement names some other revision.
// Without both, the file-scoped branch never writes the marker, which is the
// behaviour that cost a whole-repo pass on every restart after a copy.
func TestAScopedPassWritesTheWholeRepoMarkerOnlyForTheRevisionItWasArmedAt(t *testing.T) {
	const foreignSHA = "0123456789abcdef0123456789abcdef01234567"

	t.Run("armed at this checkout's HEAD, the pass promotes the marker", func(t *testing.T) {
		idx, store, repo := newCopiedPromotionFixture(t)
		head, dirty := repoHeadAndDirty(repo)
		require.NotEmpty(t, head)
		require.False(t, dirty, "the committed frontier must leave a clean tree")

		idx.armCopiedEnrichMarkerPromotion(head)
		idx.runDeferredEnrich()

		marker, found, err := store.GetEnrichmentState("r", graph.EnrichProviderRepoMarker)
		require.NoError(t, err)
		require.True(t, found,
			"the source's completion plus this frontier's completion is whole-repo "+
				"completion, and the marker is where that is recorded")
		require.Equal(t, head, marker.IndexedSHA)
		require.Empty(t, peekCopiedEnrichMarkerPromotion(idx),
			"the entitlement is consumed by the pass that used it")
	})

	t.Run("armed at another revision, the pass leaves the marker alone", func(t *testing.T) {
		idx, store, repo := newCopiedPromotionFixture(t)
		head, _ := repoHeadAndDirty(repo)
		require.NotEqual(t, foreignSHA, head)

		idx.armCopiedEnrichMarkerPromotion(foreignSHA)
		idx.runDeferredEnrich()

		_, found, err := store.GetEnrichmentState("r", graph.EnrichProviderRepoMarker)
		require.NoError(t, err)
		require.False(t, found,
			"HEAD moved out from under the armed pass, so the two proofs no longer "+
				"compose; the next restart must still re-arm the full pass")
		require.Empty(t, peekCopiedEnrichMarkerPromotion(idx),
			"a declined entitlement is consumed too, or the next unrelated scoped "+
				"pass would inherit it")
	})

	t.Run("with nothing armed, a scoped pass never touches the marker", func(t *testing.T) {
		idx, store, _ := newCopiedPromotionFixture(t)
		idx.runDeferredEnrich()

		_, found, err := store.GetEnrichmentState("r", graph.EnrichProviderRepoMarker)
		require.NoError(t, err)
		require.False(t, found,
			"the standing rule is unchanged: a file frontier does not publish the "+
				"whole-repository completion marker on its own")
	})
}

// TestADeferredTailCopyArmsNoMarkerPromotion pins the other direction: a
// WHOLE-REPO arm never entitles a promotion, whatever evidence is to hand.
//
// It does not need one. A full arm reaches EnrichAll, which records the
// completion marker itself on a clean non-partial pass — so entitling the
// scoped promotion as well would put two writers on the same row, on a path
// that already has a correct one.
func TestADeferredTailCopyArmsNoMarkerPromotion(t *testing.T) {
	t.Run("a batch-suppressed tail arms whole-repo and no promotion", func(t *testing.T) {
		mi, repo, logs := copyTrackHarness(t)
		store := harnessStore(t, mi)
		seedCompletedProvider(t, store, copySourcePrefix, "go-types")
		// A marker that WOULD entitle a scoped arm, to prove the whole-repo
		// path declines on shape rather than on missing evidence.
		writeRepoMarker(t, store, copySourcePrefix, sourceIndexedSHA(t, store, copySourcePrefix))

		wt := divergedWorktree(t, repo, "deferred")
		mi.BeginParallelBatch()
		res := trackDivergedCopy(t, mi, wt, logs)

		idx := mi.GetIndexer(res.RepoPrefix)
		require.NotNil(t, idx)
		idx.deferredEnrichMu.Lock()
		full := idx.deferredEnrichFull
		idx.deferredEnrichMu.Unlock()
		require.True(t, full, "precondition: a deferred tail arms the whole repository")
		require.Empty(t, peekCopiedEnrichMarkerPromotion(idx),
			"the whole-repo pass writes the marker itself; nothing to promote")
	})

	t.Run("a later full arm clears an entitlement already granted", func(t *testing.T) {
		mi, repo, logs := copyTrackHarness(t)
		store := harnessStore(t, mi)
		seedCompletedProvider(t, store, copySourcePrefix, "go-types")
		srcSHA := sourceIndexedSHA(t, store, copySourcePrefix)
		writeRepoMarker(t, store, copySourcePrefix, srcSHA)

		wt := divergedWorktree(t, repo, "cleared")
		res := trackDivergedCopy(t, mi, wt, logs)
		idx := mi.GetIndexer(res.RepoPrefix)
		require.NotNil(t, idx)
		require.Equal(t, gitHeadSHA(wt), peekCopiedEnrichMarkerPromotion(idx),
			"precondition: the inline scoped repair was entitled")

		// The fallback shape, handed the same evidence.
		require.NotNil(t, mi.armCopiedRepoEnrich(res.RepoPrefix, nil,
			copiedMarkerEvidence{srcSHA: srcSHA, dstSHA: gitHeadSHA(wt)}))
		require.Empty(t, peekCopiedEnrichMarkerPromotion(idx),
			"widening the arm to the whole repository must retire the scoped entitlement")
	})

	t.Run("a scoped arm that silently widens to a full pass does not inherit a marker promotion", func(t *testing.T) {
		// markPendingEnrichFiles has its own widening branch: a caller that
		// finds pendingEnrich already raised, with nothing staged and the
		// pass not yet full, cannot tell a scoped frontier from a legacy
		// atomic-only marker, so it treats the arm as full and clears
		// copiedEnrichMarkerSHA for exactly that reason. armCopiedRepoEnrich
		// must not then re-arm the promotion its own marker check would
		// otherwise grant — that would undo the widen's clear and let a
		// later, unrelated scoped pass promote a marker for a repo whose
		// full pass never actually ran to completion.
		mi, repo, logs := copyTrackHarness(t)
		store := harnessStore(t, mi)
		seedCompletedProvider(t, store, copySourcePrefix, "go-types")
		srcSHA := sourceIndexedSHA(t, store, copySourcePrefix)
		writeRepoMarker(t, store, copySourcePrefix, srcSHA)

		wt := divergedWorktree(t, repo, "widen")
		res := trackDivergedCopy(t, mi, wt, logs)
		idx := mi.GetIndexer(res.RepoPrefix)
		require.NotNil(t, idx)

		// Reset to the shape markPendingEnrichFiles treats as ambiguous:
		// pendingEnrich already raised, nothing staged, not yet full —
		// as if some earlier legacy atomic-only caller had set the gate
		// directly. Any entitlement the inline copy itself armed is
		// consumed first so it cannot be mistaken for the one under test.
		idx.takeCopiedEnrichMarkerPromotion()
		idx.deferredEnrichMu.Lock()
		idx.deferredEnrichFull = false
		idx.deferredEnrichFiles = nil
		idx.pendingEnrich.Store(true)
		idx.deferredEnrichMu.Unlock()

		dstSHA := gitHeadSHA(wt)
		require.NotNil(t, mi.armCopiedRepoEnrich(res.RepoPrefix, []string{"widened.go"},
			copiedMarkerEvidence{srcSHA: srcSHA, dstSHA: dstSHA}))

		idx.deferredEnrichMu.Lock()
		full := idx.deferredEnrichFull
		idx.deferredEnrichMu.Unlock()
		require.True(t, full,
			"precondition: markPendingEnrichFiles widened the ambiguous arm to a full pass")
		require.Empty(t, peekCopiedEnrichMarkerPromotion(idx),
			"a widened arm must not have its marker check re-arm the promotion the widen just cleared")
	})
}

// partialScopedProvider ends a FILE-SCOPED pass partial and a WHOLE-REPO pass
// cleanly — the disposition measured on the workspace this fallback was written
// for, where pyright could not load a 9,800-file project inside the 120 s save
// deadline and every one of 12 copy repairs returned partial with zero
// coverage, while the same provider's whole-repo pass took 685 s and finished.
//
// wholeRepoPartial makes the whole-repo pass partial too, which is how a test
// observes the escalated arm itself rather than the state after it completes.
type partialScopedProvider struct {
	mu               sync.Mutex
	files            []string
	repos            []string
	wholeRepoPartial bool
	scopedErr        error
}

func (p *partialScopedProvider) Name() string        { return "partial-scoped" }
func (p *partialScopedProvider) Languages() []string { return []string{"go"} }
func (p *partialScopedProvider) Available() bool     { return true }
func (p *partialScopedProvider) Close() error        { return nil }

func (p *partialScopedProvider) EnrichFile(_ graph.Store, _, filePath string) (*semantic.EnrichResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.files = append(p.files, filePath)
	if p.scopedErr != nil {
		return nil, p.scopedErr
	}
	return &semantic.EnrichResult{
		Provider:    p.Name(),
		Language:    "go",
		Partial:     true,
		AbortReason: "per-repo enrichment deadline reached",
	}, nil
}

func (p *partialScopedProvider) EnrichRepo(_ graph.Store, repoPrefix, _ string) (*semantic.EnrichResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.repos = append(p.repos, repoPrefix)
	return &semantic.EnrichResult{
		Provider: p.Name(), Language: "go", Partial: p.wholeRepoPartial,
	}, nil
}

func (p *partialScopedProvider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	return p.EnrichRepo(g, "", repoRoot)
}

func (p *partialScopedProvider) observed() (files, repos []string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.files...), append([]string(nil), p.repos...)
}

// deferredEnrichArm reads the pending arm's shape: whether it is a whole-repo
// pass, whether anything is queued at all, and whether it is a copy repair.
func deferredEnrichArm(idx *Indexer) (full, pending, repair bool) {
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	return idx.deferredEnrichFull, idx.pendingEnrich.Load(), idx.deferredEnrichRepair
}

// TestACopyRepairThatEndsPartialFallsBackToAWholeRepoPass is the fix for the
// silent stop.
//
// runDeferredEnrich used to return on result.Partial with no log, no stamp, no
// marker promotion and an in-memory arm nothing re-dispatches — so a copy whose
// scoped repair could not finish inside the save-sized deadline simply stopped,
// read `partial` until the next daemon start, and then paid a whole-repo pass
// there instead. A repair that does not complete now escalates itself, in this
// process, to the pass it would otherwise have waited a restart for.
func TestACopyRepairThatEndsPartialFallsBackToAWholeRepoPass(t *testing.T) {
	// A one-file fixture sits under the admission floor, which would gate the
	// whole-repo pass out before it reached the provider and make the escalation
	// unobservable for the wrong reason.
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")

	t.Run("the whole-repo pass runs in the same call and completes the marker", func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		provider := &partialScopedProvider{}
		idx, store, repo := newScopedEnrichFixture(t, provider, zap.New(core))
		head, dirty := repoHeadAndDirty(repo)
		require.False(t, dirty)

		idx.armCopiedEnrichRepair()
		idx.armCopiedEnrichMarkerPromotion(head)
		idx.runDeferredEnrich()

		files, repos := provider.observed()
		require.Len(t, files, 1, "the scoped repair must have been dispatched first")
		require.Equal(t, []string{"r"}, repos,
			"and its partial result must have escalated to the whole-repo pass "+
				"NOW, rather than leaving it for a restart")

		partial := logs.FilterMessage("file-scoped deferred semantic enrichment ended partial").All()
		require.Len(t, partial, 1, "a partial scoped pass must never be silent again")
		require.Equal(t, zap.WarnLevel, partial[0].Level)
		fields := partial[0].ContextMap()
		require.Equal(t, "r", fields["repo"])
		require.Equal(t, provider.Name(), fields["provider"])
		require.Equal(t, "go", fields["language"])
		require.Equal(t, int64(1), fields["files"])
		require.Equal(t, "per-repo enrichment deadline reached", fields["abort_reason"])
		require.Equal(t, true, fields["copy_repair"])
		require.Contains(t, fields, "elapsed")
		require.Contains(t, fields, "deadline")

		require.Len(t, logs.FilterMessage(
			"worktree copy: scoped repair ended partial; falling back to a whole-repo pass").All(), 1)

		marker, found, err := store.GetEnrichmentState("r", graph.EnrichProviderRepoMarker)
		require.NoError(t, err)
		require.True(t, found,
			"the whole-repo pass writes the marker itself, which is what stops the "+
				"next restart re-arming the very pass this call just ran")
		require.Equal(t, head, marker.IndexedSHA)

		require.Empty(t, peekCopiedEnrichMarkerPromotion(idx),
			"markPendingEnrichFull retires the scoped promotion: the whole-repo pass "+
				"is now the writer of that row, and two writers is one too many")
		full, pending, repair := deferredEnrichArm(idx)
		require.False(t, pending, "the completed whole-repo pass discharged the ledger")
		require.False(t, full)
		require.False(t, repair)
	})

	t.Run("an escalated pass that is itself partial leaves a whole-repo arm standing", func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		provider := &partialScopedProvider{wholeRepoPartial: true}
		idx, store, repo := newScopedEnrichFixture(t, provider, zap.New(core))
		head, _ := repoHeadAndDirty(repo)

		idx.armCopiedEnrichRepair()
		idx.armCopiedEnrichMarkerPromotion(head)
		idx.runDeferredEnrich()

		_, repos := provider.observed()
		require.Equal(t, []string{"r"}, repos)
		require.Len(t, logs.FilterMessage(
			"worktree copy: scoped repair ended partial; falling back to a whole-repo pass").All(), 1)

		_, found, err := store.GetEnrichmentState("r", graph.EnrichProviderRepoMarker)
		require.NoError(t, err)
		require.False(t, found,
			"a partial whole-repo pass writes no marker either — the fallback may "+
				"not launder an incomplete pass into a completed one")

		full, pending, repair := deferredEnrichArm(idx)
		require.True(t, full, "the escalation left a WHOLE-REPO arm, not the scoped one")
		require.True(t, pending, "and it is still owed, so the next start resumes it")
		require.False(t, repair,
			"a whole-repo arm is what a repair escalates TO; re-running it as a "+
				"scoped repair is exactly the loop this must not enter")
	})

	t.Run("an errored repair escalates the same way", func(t *testing.T) {
		core, logs := observer.New(zap.InfoLevel)
		provider := &partialScopedProvider{scopedErr: errors.New("language server died")}
		idx, store, repo := newScopedEnrichFixture(t, provider, zap.New(core))
		head, _ := repoHeadAndDirty(repo)

		idx.armCopiedEnrichRepair()
		idx.runDeferredEnrich()

		_, repos := provider.observed()
		require.Equal(t, []string{"r"}, repos,
			"an errored repair is no better off than a partial one")
		require.Len(t, logs.FilterMessage(
			"worktree copy: scoped repair failed; falling back to a whole-repo pass").All(), 1)

		marker, found, err := store.GetEnrichmentState("r", graph.EnrichProviderRepoMarker)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, head, marker.IndexedSHA)
	})
}

// TestAWatcherSaveThatEndsPartialDoesNotEscalateToAFullPass pins the other side
// of the disposition.
//
// A save's frontier is the handful of files someone just wrote; the next save
// retries it, and escalating to a whole-repo pass on every partial save would
// turn a keystroke into minutes of gopls. Only a copy repair — armed once by a
// track that has already returned, with nothing behind it to retry — escalates.
func TestAWatcherSaveThatEndsPartialDoesNotEscalateToAFullPass(t *testing.T) {
	t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")

	core, logs := observer.New(zap.InfoLevel)
	provider := &partialScopedProvider{}
	idx, store, _ := newScopedEnrichFixture(t, provider, zap.New(core))

	// No armCopiedEnrichRepair: this is the shape IncrementalReindexPaths leaves
	// behind on a save.
	idx.runDeferredEnrich()

	files, repos := provider.observed()
	require.Len(t, files, 1)
	require.Empty(t, repos, "a partial save must not drag a whole-repo pass behind it")

	partial := logs.FilterMessage("file-scoped deferred semantic enrichment ended partial").All()
	require.Len(t, partial, 1, "it is still logged — never silent, whoever armed it")
	require.Equal(t, zap.WarnLevel, partial[0].Level)
	require.Equal(t, false, partial[0].ContextMap()["copy_repair"])
	require.Empty(t, logs.FilterMessage(
		"worktree copy: scoped repair ended partial; falling back to a whole-repo pass").All())
	require.Empty(t, logs.FilterMessage(
		"file-scoped deferred semantic enrichment: copy repair deadline").All(),
		"a save keeps the configured semantic timeout, not the repo-scaled one")

	full, pending, _ := deferredEnrichArm(idx)
	require.False(t, full, "the arm stays scoped")
	require.True(t, pending, "and stays owed, so the next save's pass retries it")

	_, found, err := store.GetEnrichmentState("r", graph.EnrichProviderRepoMarker)
	require.NoError(t, err)
	require.False(t, found)
}

// TestACopyRepairArmCarriesTheScaledDeadline pins how a dispatch tells a repair
// from a save, and that the repair really runs under the whole-repo budget.
//
// The two frontiers are the same shape and the dispatcher cannot tell them
// apart by looking; only the arm knows. Getting it wrong is not a small error —
// the save-sized deadline is what made every copy repair on the measured
// workspace return partial having covered nothing.
func TestACopyRepairArmCarriesTheScaledDeadline(t *testing.T) {
	t.Run("an inline diverged copy arms a repair", func(t *testing.T) {
		mi, repo, logs := copyTrackHarness(t)
		// A scoped repair is only offered for a source whose own enrichment
		// finished: from a lagging one the copy owes the whole repository, and
		// this test would then be measuring the wrong arm. See
		// TestACopyFromALaggingSourceOwesAWholeRepoPass.
		seedCompletedProvider(t, harnessStore(t, mi), copySourcePrefix, "go-types")
		wt := divergedWorktree(t, repo, "repair")
		res := trackDivergedCopy(t, mi, wt, logs)

		idx := mi.GetIndexer(res.RepoPrefix)
		require.NotNil(t, idx)
		require.True(t, idx.deferredEnrichIsRepair(),
			"the copy path is the only caller that knows this frontier is a whole "+
				"divergence rather than a save")
	})

	t.Run("a deferred-tail copy arms a whole-repo pass and no repair", func(t *testing.T) {
		mi, repo, logs := copyTrackHarness(t)
		wt := divergedWorktree(t, repo, "tail")
		mi.BeginParallelBatch()
		res := trackDivergedCopy(t, mi, wt, logs)

		idx := mi.GetIndexer(res.RepoPrefix)
		require.NotNil(t, idx)
		full, _, repair := deferredEnrichArm(idx)
		require.True(t, full, "precondition: a deferred tail arms the whole repository")
		require.False(t, repair,
			"a whole-repo pass already runs under the repo-scaled deadline and "+
				"already writes its own marker; there is nothing for the repair "+
				"disposition to select")
	})

	t.Run("a plain file arm is a save, and a full arm retires a repair", func(t *testing.T) {
		idx := &Indexer{}
		idx.markPendingEnrichFiles([]string{"r/main.go"})
		require.False(t, idx.deferredEnrichIsRepair(),
			"markPendingEnrichFiles is the watcher's entry point; it must not "+
				"silently buy the repo-scaled budget for a save")

		idx.armCopiedEnrichRepair()
		require.True(t, idx.deferredEnrichIsRepair())

		idx.markPendingEnrichFull()
		require.False(t, idx.deferredEnrichIsRepair())
	})

	t.Run("the disposition is consumed by the dispatch that runs it", func(t *testing.T) {
		idx := &Indexer{}
		idx.markPendingEnrichFiles([]string{"r/main.go"})
		idx.armCopiedEnrichRepair()

		files, full, _, repair := idx.takeDeferredEnrichScope()
		require.Equal(t, []string{"r/main.go"}, files)
		require.False(t, full)
		require.True(t, repair)

		_, _, _, repair = idx.takeDeferredEnrichScope()
		require.False(t, repair,
			"the NEXT dispatch over this frontier is an ordinary scoped pass — a "+
				"watcher save that merged into it, say — and must not inherit the "+
				"repair's budget or its escalation")
		require.False(t, idx.deferredEnrichIsRepair())
	})

	t.Run("a dispatched repair runs under the repo-scaled deadline", func(t *testing.T) {
		t.Setenv("GORTEX_ENRICH_MIN_NODES", "0")
		core, logs := observer.New(zap.InfoLevel)
		provider := &partialScopedProvider{}
		idx, _, _ := newScopedEnrichFixture(t, provider, zap.New(core))

		idx.armCopiedEnrichRepair()
		idx.runDeferredEnrich()

		announced := logs.FilterMessage(
			"file-scoped deferred semantic enrichment: copy repair deadline").All()
		require.Len(t, announced, 1, "the budget a repair was given must be in the log")
		fields := announced[0].ContextMap()
		nodes := idx.semanticMgr.RepoEnrichableNodes(idx.graph, "r", idx.rootPath)
		saveDeadline := idx.semanticMgr.ScopedEnrichTimeout()
		repoDeadline := idx.semanticMgr.RepoEnrichDeadline(nodes)
		require.Equal(t, saveDeadline, fields["save_deadline"])
		require.Equal(t, repoDeadline, fields["repo_deadline"],
			"the uncapped repo-scaled sizing the dispatched deadline was capped from "+
				"must still be in the log")
		require.Equal(t, idx.semanticMgr.CopyRepairDeadline(nodes), fields["deadline"],
			"a repair's dispatched deadline is the repo-scaled sizing capped near "+
				"the save deadline, not the uncapped repo sizing")
		require.LessOrEqual(t, fields["deadline"], 5*saveDeadline,
			"the cap binds here: this tiny fixture's repo-scaled deadline is the "+
				"10-minute floor, far past 5x the fixture's 10s save timeout")
		require.Less(t, fields["deadline"], repoDeadline,
			"proves the cap actually bound something rather than coinciding with "+
				"the uncapped repo value")
	})
}
