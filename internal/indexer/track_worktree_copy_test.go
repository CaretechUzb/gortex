package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// copyGateGraph is the in-memory graph plus the one question
// worktreeCopySource actually ranks on: which commit a tracked repository's
// subgraph describes, and whether its tree was dirty when that subgraph was
// written.
//
// *graph.Graph does NOT implement graph.RepoIndexStateReader — only the sqlite
// store does — and copySourceCommit declines any candidate whose indexed commit
// it cannot read. Without this wrapper every test below would decline on the
// BACKEND rather than on the checkout, and would pass for the wrong reason.
type copyGateGraph struct {
	*graph.Graph
	state map[string]graph.RepoIndexState
}

func (g *copyGateGraph) GetRepoIndexState(prefix string) (graph.RepoIndexState, bool, error) {
	st, ok := g.state[prefix]
	return st, ok, nil
}

// copyGateIndexer builds an indexer that knows about one already-tracked
// checkout at root, which is what worktreeCopySource scans for a source.
//
// The source's subgraph is declared to describe root's CURRENT HEAD, clean —
// the ordinary case of a freshly indexed checkout. Tests that need a stale or
// dirty source say so with copyGateIndexerAt.
func copyGateIndexer(t *testing.T, prefix, root string) *MultiIndexer {
	t.Helper()
	return copyGateIndexerAt(t, prefix, root, gitHeadSHA(root), false)
}

// copyGateIndexerAt spells out the source's persisted index state.
//
// indexedSHA is the commit the source's ROWS describe, which is not necessarily
// the commit its checkout is sitting on — that gap is the whole subject of
// copySourceCommit. An empty indexedSHA with dirty=false records no state at
// all, i.e. a repository the store has never indexed.
func copyGateIndexerAt(t *testing.T, prefix, root, indexedSHA string, dirty bool) *MultiIndexer {
	t.Helper()
	mi := copyGateIndexerNoState(t, prefix, root)
	if indexedSHA != "" || dirty {
		mi.graph.(*copyGateGraph).state[prefix] = graph.RepoIndexState{
			RepoPrefix: prefix,
			IndexedSHA: indexedSHA,
			Dirty:      dirty,
		}
	}
	return mi
}

// copyGateIndexerNoState is the same indexer with an empty state table: the
// backend can answer, and answers "never indexed".
func copyGateIndexerNoState(t *testing.T, prefix, root string) *MultiIndexer {
	t.Helper()
	return &MultiIndexer{
		graph:    &copyGateGraph{Graph: graph.New(), state: map[string]graph.RepoIndexState{}},
		repos:    map[string]*RepoMetadata{prefix: {RepoPrefix: prefix, RootPath: root}},
		indexers: map[string]*Indexer{prefix: {repoPrefix: prefix}},
		logger:   zap.NewNop(),
	}
}

// addTrackedWorktree adds a second candidate to mi: a linked worktree of the
// same repository, tracked under its own prefix, with its own declared index
// state. Returns the checkout path.
func addTrackedWorktree(t *testing.T, mi *MultiIndexer, repo, prefix, branch, indexedSHA string) string {
	t.Helper()
	wt := addWorktree(t, repo, branch)
	mi.repos[prefix] = &RepoMetadata{RepoPrefix: prefix, RootPath: wt}
	mi.indexers[prefix] = &Indexer{repoPrefix: prefix}
	mi.graph.(*copyGateGraph).state[prefix] = graph.RepoIndexState{
		RepoPrefix: prefix,
		IndexedSHA: indexedSHA,
	}
	return wt
}

// addWorktree checks out branch as a linked worktree of repo and returns its
// path, symlink-resolved so macOS's /var aliasing cannot fail a comparison.
func addWorktree(t *testing.T, repo, branch string) string {
	t.Helper()
	wt := filepath.Join(t.TempDir(), branch)
	runGit(t, repo, "worktree", "add", "-q", "-b", branch, wt)
	return realpath(t, wt)
}

// The historical case, unchanged: two checkouts at the same commit are the same
// code, so the copy stands alone and there is nothing to reconcile.
func TestCopySourceAtTheSameCommitReportsNothingChanged(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	wt := addWorktree(t, repo, "same")

	mi := copyGateIndexer(t, "base", repo)
	src, changed, ok := mi.worktreeCopySource(wt)

	require.True(t, ok, "a sibling checkout at the same commit must be copyable")
	require.Equal(t, "base", src)
	require.Empty(t, changed, "identical checkouts disagree on nothing")
}

// The case the gate used to refuse outright, and the one that cost the most: a
// merge-request worktree a few commits off its base. Rejecting it meant
// re-parsing and re-deriving the whole repository to learn a handful of files —
// measured at 667s against roughly 200s for copy plus reconcile.
func TestCopySourceAcceptsASmallDivergenceAndNamesTheChangedFiles(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	wt := addWorktree(t, repo, "feature")

	writeFile(t, filepath.Join(wt, "b.go"), "package main\n")
	writeFile(t, filepath.Join(wt, "a.go"), "package main // edited\n")
	runGit(t, wt, "add", ".")
	runGit(t, wt, "commit", "-q", "-m", "feature")

	mi := copyGateIndexer(t, "base", repo)
	src, changed, ok := mi.worktreeCopySource(wt)

	require.True(t, ok, "a small divergence must not fall back to a cold index")
	require.Equal(t, "base", src)
	require.Equal(t, []string{"a.go", "b.go"}, changed,
		"both the edited and the added file must reach the reconcile; a path "+
			"missing here keeps the source's nodes under this prefix forever")
}

// Uncommitted work is the same hazard as a committed difference — the copy
// installs the SOURCE's graph, so a locally modified file would be described by
// nodes that do not match disk. The same-HEAD gate used to exclude that case
// wholesale; relaxing it means taking responsibility for it.
func TestCopySourceReportsUncommittedWorkAsChanged(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	wt := addWorktree(t, repo, "dirty")

	// HEAD still matches the source, so only the working tree differs.
	writeFile(t, filepath.Join(wt, "a.go"), "package main // uncommitted\n")
	writeFile(t, filepath.Join(wt, "untracked.go"), "package main\n")

	mi := copyGateIndexer(t, "base", repo)
	_, changed, ok := mi.worktreeCopySource(wt)

	require.True(t, ok)
	// Same HEAD short-circuits before any diff, which is what keeps the
	// historical path free of git work — so dirtiness is invisible here.
	// Pinned as the known limit of this gate, not asserted as desirable.
	require.Empty(t, changed,
		"documented gap: an identical HEAD short-circuits before the diff, so "+
			"uncommitted edits are left to the watcher, exactly as before this change")
}

// Beyond the cap the copy declines and indexing takes over. Indexing is always
// correct, only slower, so the cap is free to be conservative.
func TestCopySourceDeclinesBeyondTheDivergenceCap(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	wt := addWorktree(t, repo, "big")

	writeFile(t, filepath.Join(wt, "b.go"), "package main\n")
	writeFile(t, filepath.Join(wt, "c.go"), "package main\n")
	runGit(t, wt, "add", ".")
	runGit(t, wt, "commit", "-q", "-m", "big")

	original := worktreeCopyMaxDivergence
	t.Cleanup(func() { worktreeCopyMaxDivergence = original })
	worktreeCopyMaxDivergence = 1

	mi := copyGateIndexer(t, "base", repo)
	_, _, ok := mi.worktreeCopySource(wt)
	require.False(t, ok, "a divergence over the cap must fall back to indexing")

	worktreeCopyMaxDivergence = 2
	_, changed, ok := mi.worktreeCopySource(wt)
	require.True(t, ok, "exactly at the cap is still a copy")
	require.Len(t, changed, 2)
}

// Same checkout group is the condition nothing substitutes for: it is what
// makes the destination entitled to the source's bindings. An unrelated
// repository that happens to sit nearby is not a copy source at any distance.
func TestCopySourceRefusesAnUnrelatedRepository(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	wt := addWorktree(t, repo, "branch")

	other := realpath(t, t.TempDir())
	initTestRepo(t, other, "main")

	mi := copyGateIndexer(t, "unrelated", other)
	_, _, ok := mi.worktreeCopySource(wt)
	require.False(t, ok, "a different repository shares no checkout group")
}

// The regression this pins cost 20 stranded nodes in production. A worktree
// eight files off its copy source kept every node of the one file that branch
// deleted, and the reconcile reported `deleted: 0` — because restat had dropped
// the path from the ledger, and the reconcile's deleted set is the subset of
// the ledger it cannot stat. A dropped path is one it never looks at.
func TestRestatKeepsAPathThisCheckoutDoesNotHave(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(root, "present.py"), []byte("x = 1\n"), 0o644))

	// The ledger the copy carried: one file this checkout has, one it does not.
	copied := map[string]int64{"present.py": 111, "gone.py": 222}
	mtimes, missing := restatWorktreeMtimes(root, copied)

	require.Contains(t, mtimes, "gone.py",
		"a path absent from disk must stay in the ledger, or nothing evicts its copied nodes")
	require.True(t, missing["gone.py"])
	require.False(t, missing["present.py"])
	require.NotEqual(t, int64(111), mtimes["present.py"],
		"a file that exists is restat'd, not left on the source's mtime")
}

// The other half of the same rule, and the half that reintroduces the bug if
// someone simplifies the loop back to an unconditional delete.
func TestWithholdKeepsDeletedPathsAndDropsModifiedOnes(t *testing.T) {
	mtimes := map[string]int64{"kept.py": 1, "edited.py": 2, "gone.py": 3, "untouched.py": 4}
	changed := []string{"edited.py", "gone.py"}
	missing := map[string]bool{"gone.py": true}

	withholdReconciledPaths(mtimes, changed, missing)

	require.NotContains(t, mtimes, "edited.py",
		"a changed file present on disk must read as never indexed so it is reindexed")
	require.Contains(t, mtimes, "gone.py",
		"a changed file absent from disk must stay, because that entry is what triggers eviction")
	require.Contains(t, mtimes, "untouched.py")
	require.Contains(t, mtimes, "kept.py")
}

// A stat that fails for any reason OTHER than the file being absent is not
// evidence of deletion, and evicting on it would drop a file that exists. The
// unreadable-parent trick does not hold as root, where every stat succeeds.
func TestRestatDoesNotTreatAnUnreadablePathAsDeleted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so no stat error can be induced")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	require.NoError(t, os.Mkdir(locked, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(locked, "hidden.py"), []byte("x = 1\n"), 0o644))
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	_, missing := restatWorktreeMtimes(root, map[string]int64{"locked/hidden.py": 1})

	require.False(t, missing["locked/hidden.py"],
		"a permissions fault must not be reported as a deletion")
}

// scheduleCopiedRepoEnrich is the caller a diverged copy was missing: without
// it the repository carries the SOURCE's enrichment_state rows, reads "partial"
// — which blocks queries — and never recovers, because the only other caller of
// MaybeSeedPendingEnrich runs from daemon warmup. Measured before the fix: 801s
// of further daemon activity left MIN(enrichment content_gen) at 1 against a
// repo content_gen of 4.
func TestCopiedRepoEnrichIsANoOpForAnUnknownPrefix(t *testing.T) {
	mi := copyGateIndexer(t, "base", realpath(t, t.TempDir()))
	require.NotPanics(t, func() { mi.scheduleCopiedRepoEnrich("no-such-prefix", nil) },
		"the copy path names a prefix the indexer map may not carry; a panic here kills the track")
}

// The gate must be armed unconditionally, NOT by asking MaybeSeedPendingEnrich.
//
// That was the first attempt, and production proved it a silent no-op: the
// predicate needs a __repo__ completion marker to conclude anything, and
// RecordRepoEnrichmentComplete never writes one for a dirty tree, so copying
// from a source with a single untracked file carried no marker and the pass was
// never armed. The repo went straight back to `partial`. Three of the six repos
// in the workspace this was written for are dirty at any given moment.
//
// This test needs no semantic manager, which is the point: the call site knows
// the rows came from another checkout, so it does not have to infer anything.
func TestCopiedRepoEnrichArmsTheGateUnconditionally(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")

	mi := copyGateIndexer(t, "base", repo)
	idx := mi.indexers["base"]
	idx.rootPath = repo
	idx.graph = mi.graph
	idx.logger = zap.NewNop()

	require.False(t, idx.pendingEnrich.Load(), "precondition: the gate starts closed")

	mi.scheduleCopiedRepoEnrich("base", nil)

	require.True(t, idx.pendingEnrich.Load(),
		"a diverged copy carries another checkout's enrichment rows, so the pass "+
			"is owed no matter what the copied marker does or does not say")
}

// A plain checkout is not a worktree of anything, so there is nothing to copy
// from even when a sibling repository is tracked.
func TestCopySourceRefusesANonWorktree(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")

	mi := copyGateIndexer(t, "base", repo)
	_, _, ok := mi.worktreeCopySource(repo)
	require.False(t, ok)
}

// The bug this whole change exists for. worktreeCopySource used to rank on the
// source's live HEAD, but the rows it copies were written by the source's last
// INDEX — so a source whose HEAD has moved since was compared against a tree its
// graph does not describe. Measured in production: 273 reconciled paths where
// the copied graph was 20 away, 253 files reindexed for nothing.
func TestCopySourceRanksOnTheIndexedCommitNotHEAD(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	indexed := gitHeadSHA(repo)
	wt := addWorktree(t, repo, "feature")

	// The source's checkout advances well past what its graph describes.
	for _, name := range []string{"moved1.go", "moved2.go", "moved3.go"} {
		writeFile(t, filepath.Join(repo, name), "package main\n")
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "source advances past its index")

	// The destination differs from the INDEXED commit by exactly one file.
	writeFile(t, filepath.Join(wt, "b.go"), "package main\n")
	runGit(t, wt, "add", ".")
	runGit(t, wt, "commit", "-q", "-m", "feature")

	mi := copyGateIndexerAt(t, "base", repo, indexed, false)
	src, changed, ok := mi.worktreeCopySource(wt)

	require.True(t, ok)
	require.Equal(t, "base", src)
	require.Equal(t, []string{"b.go"}, changed,
		"the reconcile set must be measured against the commit the copied rows "+
			"describe; ranking on the source's HEAD would have named its three "+
			"advanced files too and reindexed them for nothing")
}

// The fail-open half, and the reason this is a correctness fix rather than an
// optimisation. A file that differs from the source's INDEXED commit but happens
// to match the source's HEAD is absent from a HEAD-ranked diff — and
// restatWorktreeMtimes then records it as current, so the reconcile never
// revisits it and the source's stale nodes stand under this prefix forever.
func TestCopySourceKeepsAFileMatchingSourceHEADButNotItsGraph(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	indexed := gitHeadSHA(repo)
	wt := addWorktree(t, repo, "converged")

	// Source advances a.go to v2 AFTER the index that produced its rows.
	writeFile(t, filepath.Join(repo, "a.go"), "package main // v2\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "source advances a.go")

	// The destination independently reaches the SAME content on its own commit.
	writeFile(t, filepath.Join(wt, "a.go"), "package main // v2\n")
	runGit(t, wt, "add", ".")
	runGit(t, wt, "commit", "-q", "-m", "same content, different commit")

	mi := copyGateIndexerAt(t, "base", repo, indexed, false)
	_, changed, ok := mi.worktreeCopySource(wt)

	require.True(t, ok)
	require.Contains(t, changed, "a.go",
		"a.go matches the source's HEAD, so a HEAD-ranked diff omits it — but the "+
			"copied rows hold the pre-advance content, and a path left out of "+
			"`changed` is one the reconcile never looks at again")
}

// A dirty source is refused outright. RepoIndexState.Dirty records THAT the tree
// was dirty when indexed, never WHICH files, so no commit-to-commit diff covers
// the difference and asking git today answers about the tree today. Measured
// consequence of copying one anyway: the destination's indexed content_hash
// matched its dirty source at 15,642 bytes while its own checkout held the
// committed file at 15,596.
func TestCopySourceDeclinesASourceDirtyWhenItWasIndexed(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	wt := addWorktree(t, repo, "branch")

	mi := copyGateIndexerAt(t, "base", repo, gitHeadSHA(repo), true)
	_, _, ok := mi.worktreeCopySource(wt)

	require.False(t, ok,
		"a source dirty at index time describes a working tree nobody recorded; "+
			"declining costs an ordinary index, copying costs a wrong graph")
}

// Even at the identical commit. The fast path returns before any diff, so a
// dirty source taken there would be restamped `ready` over rows describing
// uncommitted content — the worst reachable outcome of this gate.
func TestCopySourceDeclinesADirtySourceEvenAtTheSameCommit(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	wt := addWorktree(t, repo, "same")

	mi := copyGateIndexerAt(t, "base", repo, gitHeadSHA(wt), true)
	_, _, ok := mi.worktreeCopySource(wt)

	require.False(t, ok,
		"the identical fast path must consult Dirty before short-circuiting, or "+
			"it restamps a dirty-source graph as ready")
}

// No index state at all: the store has never indexed this candidate, so nothing
// says what its rows describe. Decline — never fall back to HEAD, which is the
// unsound proxy the whole change removes.
func TestCopySourceDeclinesWhenTheIndexStateIsUnknown(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	wt := addWorktree(t, repo, "branch")

	mi := copyGateIndexerNoState(t, "base", repo)
	_, _, ok := mi.worktreeCopySource(wt)

	require.False(t, ok,
		"an unknown indexed commit must decline, not silently rank on HEAD")
}

// The backend cannot answer the question at all — the in-memory *graph.Graph
// implements no RepoIndexStateReader. Same verdict, different cause: an
// optimisation is lost, correctness is not.
func TestCopySourceDeclinesWhenTheBackendCannotAnswer(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	wt := addWorktree(t, repo, "branch")

	mi := &MultiIndexer{
		graph:    graph.New(),
		repos:    map[string]*RepoMetadata{"base": {RepoPrefix: "base", RootPath: repo}},
		indexers: map[string]*Indexer{"base": {repoPrefix: "base"}},
		logger:   zap.NewNop(),
	}
	_, _, ok := mi.worktreeCopySource(wt)

	require.False(t, ok,
		"a backend that cannot report an indexed commit gets no copy; indexing "+
			"is always correct and only slower")
}

// The source's indexed commit was rebased away or garbage collected, so this
// checkout cannot resolve it. gitChangedPaths fails, and there is no sound
// fallback — HEAD is exactly what must not be substituted here.
func TestCopySourceDeclinesAnIndexedCommitThisCheckoutCannotResolve(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	wt := addWorktree(t, repo, "branch")

	mi := copyGateIndexerAt(t, "base", repo, "0123456789abcdef0123456789abcdef01234567", false)
	_, _, ok := mi.worktreeCopySource(wt)

	require.False(t, ok,
		"an unresolvable indexed commit must decline rather than diff against HEAD")
}

// The free case now keys on the INDEXED commit, not the checkout's HEAD: the
// source's rows already describe this code even though its working tree has
// moved on. Under the old rule this diffed; it should short-circuit.
func TestCopySourceIdenticalPathKeysOnTheIndexedCommit(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	indexed := gitHeadSHA(repo)
	wt := addWorktree(t, repo, "atindexed")

	// The source checkout advances; its rows do not.
	writeFile(t, filepath.Join(repo, "later.go"), "package main\n")
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "source moves on")

	mi := copyGateIndexerAt(t, "base", repo, indexed, false)
	src, changed, ok := mi.worktreeCopySource(wt)

	require.True(t, ok)
	require.Equal(t, "base", src)
	require.Empty(t, changed,
		"the copied rows describe exactly this commit, so there is nothing to "+
			"reconcile — the source's checkout having moved on is irrelevant")
}

// Staleness is not a disqualifier, and this is the shape that proves it: the
// production copy chose a 22h-stale worktree 20 files away over the fresh main
// checkout 1,498 files away. Ranking is on true divergence from what each
// candidate's rows describe, nothing else.
func TestCopySourcePrefersAStaleCandidateThatIsActuallyCloser(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")
	base := gitHeadSHA(repo)

	// The destination sits two files off `base`.
	dest := addWorktree(t, repo, "dest")
	writeFile(t, filepath.Join(dest, "d1.go"), "package main\n")
	writeFile(t, filepath.Join(dest, "d2.go"), "package main\n")
	runGit(t, dest, "add", ".")
	runGit(t, dest, "commit", "-q", "-m", "dest")
	destHead := gitHeadSHA(dest)

	// A STALE sibling whose rows describe destHead exactly, though its own
	// checkout has since moved somewhere else entirely.
	mi := copyGateIndexerAt(t, "fresh", repo, base, false)
	stale := addTrackedWorktree(t, mi, repo, "stale", "stalebranch", destHead)
	writeFile(t, filepath.Join(stale, "wandered.go"), "package main\n")
	runGit(t, stale, "add", ".")
	runGit(t, stale, "commit", "-q", "-m", "stale checkout wanders")

	src, changed, ok := mi.worktreeCopySource(dest)

	require.True(t, ok)
	require.Equal(t, "stale", src,
		"the stale candidate's ROWS describe this checkout exactly; the fresh "+
			"one's are two files away. Divergence is measured against what was "+
			"indexed, so stale wins")
	require.Empty(t, changed)
}

// A diverged copy whose reconcile ran its synchronous scoped tail owes no
// repo-wide rederive: that pass re-derived edges the copy already carried, at
// 373–1,034 s per track on the workspace this was measured in. Both halves of
// the predicate are load-bearing — a deferred tail or an empty frontier means
// nothing was re-derived, and restamping then would bless a silently
// under-derived graph.
func TestCopiedDivergenceRepairedRequiresBothTheTailAndAFrontier(t *testing.T) {
	require.False(t, copiedDivergenceRepaired(nil))
	require.False(t, copiedDivergenceRepaired(&IndexResult{DerivedTailRan: true, StaleFileCount: 3}),
		"an empty frontier despite stale work means nothing was re-derived; the fallback still owes the work")
	require.False(t, copiedDivergenceRepaired(&IndexResult{
		StaleFileCount:      3,
		DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"base/a.py"}},
	}), "a tail deferred to a batch transition has not run; the fallback still owes the work")
	require.True(t, copiedDivergenceRepaired(&IndexResult{
		DerivedTailRan:      true,
		DerivedInvalidation: DerivedInvalidationPlan{Files: []string{"base/a.py"}},
	}))
}

// A divergence made only of non-indexable files (.po translations, assets)
// reconciles to zero stale work: the graph is bit-identical to the carried
// copy, so the copy owes neither a derive nor an enrichment — exactly the
// identical-copy shape, reached through the diverged branch. Weblate commits
// make this the COMMON case on the workspace this was built for.
func TestCopiedDivergenceRepairedAcceptsProvenNoWork(t *testing.T) {
	require.True(t, copiedDivergenceRepaired(&IndexResult{}),
		"zero stale, zero deleted, empty plan — nothing is owed regardless of the tail")
	require.False(t, copiedDivergenceRepaired(&IndexResult{FullRetrack: true}),
		"a full retrack enumerates nothing; its StaleFileCount 0 is not proof of no work")
	require.False(t, copiedDivergenceRepaired(&IndexResult{DeletedFileCount: 1}),
		"a deletion is real work; with no frontier the fallback still owes it")
}

// The repaired path hands scheduleCopiedRepoEnrich the exact reconciled
// frontier. Dirty sources are refused upstream, so every carried enrichment
// row outside that frontier describes content this checkout really has —
// re-enriching the whole repository would re-derive semantic rows the copy
// already holds, for minutes instead of seconds.
func TestCopiedRepoEnrichNarrowsToAGivenFrontier(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")

	mi := copyGateIndexer(t, "base", repo)
	idx := mi.indexers["base"]
	idx.rootPath = repo
	idx.graph = mi.graph
	idx.logger = zap.NewNop()

	mi.scheduleCopiedRepoEnrich("base", []string{"base/models/hr.py"})

	require.True(t, idx.pendingEnrich.Load())
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	require.False(t, idx.deferredEnrichFull,
		"a known frontier must arm a file-scoped pass, not a whole-repo one")
	require.Contains(t, idx.deferredEnrichFiles, "base/models/hr.py")
}

// An empty frontier is the fallback path, where nothing has vouched for the
// carried rows — the arming must stay whole-repo.
func TestCopiedRepoEnrichWithoutAFrontierArmsTheFullPass(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")

	mi := copyGateIndexer(t, "base", repo)
	idx := mi.indexers["base"]
	idx.rootPath = repo
	idx.graph = mi.graph
	idx.logger = zap.NewNop()

	mi.scheduleCopiedRepoEnrich("base", nil)

	require.True(t, idx.pendingEnrich.Load())
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	require.True(t, idx.deferredEnrichFull,
		"with no frontier the carried rows are unvouched-for; only a full pass is sound")
}
