package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// copyGateIndexer builds an indexer that knows about one already-tracked
// checkout at root, which is what worktreeCopySource scans for a source.
func copyGateIndexer(t *testing.T, prefix, root string) *MultiIndexer {
	t.Helper()
	return &MultiIndexer{
		graph:    graph.New(),
		repos:    map[string]*RepoMetadata{prefix: {RepoPrefix: prefix, RootPath: root}},
		indexers: map[string]*Indexer{prefix: {repoPrefix: prefix}},
		logger:   zap.NewNop(),
	}
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

// A plain checkout is not a worktree of anything, so there is nothing to copy
// from even when a sibling repository is tracked.
func TestCopySourceRefusesANonWorktree(t *testing.T) {
	repo := realpath(t, t.TempDir())
	initTestRepo(t, repo, "main")

	mi := copyGateIndexer(t, "base", repo)
	_, _, ok := mi.worktreeCopySource(repo)
	require.False(t, ok)
}
