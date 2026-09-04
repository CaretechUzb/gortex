package indexer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// gitWatcherFixture builds a repo whose main branch holds a.go and whose
// two feature branches each replace it with a file of their own, so every
// checkout between them produces a non-empty diff.
func gitWatcherFixture(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	repoDir := t.TempDir()
	runGit(t, repoDir, "init", "-q", "-b", "main")
	runGit(t, repoDir, "config", "user.email", "test@example.com")
	runGit(t, repoDir, "config", "user.name", "Test")
	runGit(t, repoDir, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(repoDir, "a.go"), "package main\nfunc Alpha() {}\n")
	runGit(t, repoDir, "add", ".")
	runGit(t, repoDir, "commit", "-q", "-m", "main: Alpha")

	for _, b := range []struct{ branch, file, fn string }{
		{"feature-one", "b.go", "Beta"},
		{"feature-two", "c.go", "Gamma"},
	} {
		runGit(t, repoDir, "checkout", "-q", "-b", b.branch, "main")
		require.NoError(t, os.Remove(filepath.Join(repoDir, "a.go")))
		writeFile(t, filepath.Join(repoDir, b.file), "package main\nfunc "+b.fn+"() {}\n")
		runGit(t, repoDir, "add", "-A")
		runGit(t, repoDir, "commit", "-q", "-m", b.branch)
	}
	runGit(t, repoDir, "checkout", "-q", "main")
	return repoDir
}

// startWatchedIndex indexes watchRoot and attaches a started GitWatcher to it,
// returning the graph and a channel that receives one value per reconcile.
func startWatchedIndex(t *testing.T, indexRoot, watchRoot string) (*graph.Graph, <-chan int) {
	t.Helper()
	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(indexRoot)
	_, err := idx.IndexCtx(testCtx(), indexRoot)
	require.NoError(t, err)

	gw, err := NewGitWatcher(watchRoot, idx, zap.NewNop())
	require.NoError(t, err)
	gw.debounce = 50 * time.Millisecond
	drained := make(chan int, 4)
	gw.drained = func(n int) {
		select {
		case drained <- n:
		default:
		}
	}
	require.NoError(t, gw.Start())
	t.Cleanup(func() { _ = gw.Stop() })
	return g, drained
}

func headSHA(t *testing.T, dir string) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.Output()
	require.NoError(t, err)
	return strings.TrimSpace(string(out))
}

func awaitReconcile(t *testing.T, drained <-chan int, what string) {
	t.Helper()
	select {
	case n := <-drained:
		assert.GreaterOrEqual(t, n, 1, "%s must touch at least one file", what)
	case <-time.After(10 * time.Second):
		t.Fatalf("git watcher did not reconcile %s within timeout", what)
	}
}

// TestGitWatcher_ConsecutiveCheckoutsReconcile pins down that the watcher
// keeps hearing checkouts, not just the first one.
//
// Git never writes HEAD in place: it writes HEAD.lock and renames it over
// HEAD, which unlinks the inode a file watch is registered on. fsnotify
// re-establishes the watch from the parent directory, so this works — but
// nothing in the package asserted it, and every existing test switches
// branches exactly once, which cannot tell a durable watch from a one-shot
// one. Two switches can.
func TestGitWatcher_ConsecutiveCheckoutsReconcile(t *testing.T) {
	repoDir := gitWatcherFixture(t)
	g, drained := startWatchedIndex(t, repoDir, repoDir)
	require.NotEmpty(t, g.GetFileNodes("a.go"), "main must be indexed before the switch")

	runGit(t, repoDir, "checkout", "-q", "feature-one")
	awaitReconcile(t, drained, "the first checkout")
	assert.NotEmpty(t, g.GetFileNodes("b.go"), "feature-one's Beta must be indexed")

	runGit(t, repoDir, "checkout", "-q", "feature-two")
	awaitReconcile(t, drained, "the second checkout")
	assert.NotEmpty(t, g.GetFileNodes("c.go"), "feature-two's Gamma must be indexed")
	assert.Empty(t, g.GetFileNodes("b.go"), "feature-one's Beta must be evicted")
}

// TestGitWatcher_LinkedWorktreeReconciles covers the layout the copy-install
// path produces: a linked worktree whose .git is a file pointing at
// .git/worktrees/<name>. That directory holds HEAD but neither refs/heads nor
// packed-refs — both live in the common dir — so Start subscribes to exactly
// one of the three names it looks for. This asserts that HEAD alone is enough
// to carry a checkout, which is the whole of the coverage this layout had.
func TestGitWatcher_LinkedWorktreeReconciles(t *testing.T) {
	repoDir := gitWatcherFixture(t)
	wtDir := filepath.Join(t.TempDir(), "wt")
	// --detach because feature-one must not be checked out anywhere else
	// when the worktree switches onto it below.
	runGit(t, repoDir, "worktree", "add", "-q", "--detach", wtDir, "main")

	gitFile := filepath.Join(wtDir, ".git")
	info, err := os.Stat(gitFile)
	require.NoError(t, err)
	require.False(t, info.IsDir(), "fixture must produce a linked worktree, not a nested .git dir")

	g, drained := startWatchedIndex(t, wtDir, wtDir)
	require.NotEmpty(t, g.GetFileNodes("a.go"), "the worktree must be indexed at main")

	runGit(t, wtDir, "checkout", "-q", "feature-one")
	awaitReconcile(t, drained, "the worktree checkout")
	assert.NotEmpty(t, g.GetFileNodes("b.go"), "feature-one's Beta must be indexed in the worktree")
	assert.Empty(t, g.GetFileNodes("a.go"), "main's Alpha must be evicted from the worktree")

	runGit(t, wtDir, "checkout", "-q", "feature-two")
	awaitReconcile(t, drained, "the second worktree checkout")
	assert.NotEmpty(t, g.GetFileNodes("c.go"), "feature-two's Gamma must be indexed in the worktree")
}

// TestGitWatcher_SeedsFromIndexedCommitNotHead is the regression test for a
// repository that goes stale and has no way back.
//
// A checkout that lands while no watcher is running — across a daemon
// restart, or in the gap between tracking a repo and starting its watcher —
// leaves HEAD ahead of the graph. Seeding the watcher from HEAD hides that
// gap permanently: every later event reads the SHA the watcher was already
// seeded with, takes no diff, and returns. The graph keeps the old commit's
// content, nothing restamps the freshness row, and the repo reads stale for
// the life of the daemon. Seeding from the commit the graph was actually
// built at makes the gap visible, and the catch-up closes it with no further
// checkout needed.
func TestGitWatcher_SeedsFromIndexedCommitNotHead(t *testing.T) {
	repoDir := gitWatcherFixture(t)
	indexedSHA := headSHA(t, repoDir)

	// The in-memory graph carries no freshness row, so the seed can only be
	// exercised against a store that persists one.
	st, err := store_sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	idx := New(st, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(repoDir)
	_, err = idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)
	require.NoError(t, st.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: idx.repoPrefix,
		IndexedSHA: indexedSHA,
		IndexedAt:  time.Now().Unix(),
	}))

	// The checkout happens with no watcher running: this is the whole point.
	runGit(t, repoDir, "checkout", "-q", "feature-one")
	head := headSHA(t, repoDir)
	require.NotEqual(t, indexedSHA, head, "the fixture must leave HEAD ahead of the graph")

	gw, err := NewGitWatcher(repoDir, idx, zap.NewNop())
	require.NoError(t, err)
	gw.debounce = 50 * time.Millisecond
	drained := make(chan int, 4)
	gw.drained = func(n int) {
		select {
		case drained <- n:
		default:
		}
	}

	assert.Equal(t, indexedSHA, gw.seedSHA(head),
		"the diff base must be the commit the graph was built at, not HEAD")

	require.NoError(t, gw.Start())
	t.Cleanup(func() { _ = gw.Stop() })
	awaitReconcile(t, drained, "the startup catch-up")
}

// --- linked-worktree commit freshness -------------------------------------
//
// The tests above cover CHECKOUTS in a linked worktree, which work because a
// checkout rewrites the worktree's own HEAD file. A commit on the branch
// already checked out does not: HEAD stays the symref it was, the ref moves in
// the COMMON dir, and the HEAD reflog is appended in the worktree gitdir.
// Watching only the worktree gitdir therefore left commit freshness dead --
// the repo answered every query correctly from a current graph while
// `gortex repos` reported it stale for the life of the daemon.

// startWatchedStoreIndex is startWatchedIndex over a persistent store, because
// the in-memory graph carries no freshness row and IndexedSHA is the only
// assertion that can distinguish this bug: in the live failure the graph
// content is already current via the file watcher, so asserting on nodes
// passes for the wrong reason.
func startWatchedStoreIndex(t *testing.T, indexRoot, watchRoot string) (*store_sqlite.Store, *Indexer, <-chan int) {
	t.Helper()
	st, err := store_sqlite.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = st.Close() })

	idx := New(st, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(indexRoot)
	_, err = idx.IndexCtx(testCtx(), indexRoot)
	require.NoError(t, err)
	require.NoError(t, st.SetRepoIndexState(graph.RepoIndexState{
		RepoPrefix: idx.repoPrefix,
		IndexedSHA: headSHA(t, watchRoot),
		IndexedAt:  time.Now().Unix(),
	}))

	gw, err := NewGitWatcher(watchRoot, idx, zap.NewNop())
	require.NoError(t, err)
	gw.debounce = 50 * time.Millisecond
	drained := make(chan int, 4)
	gw.drained = func(n int) {
		select {
		case drained <- n:
		default:
		}
	}
	require.NoError(t, gw.Start())
	t.Cleanup(func() { _ = gw.Stop() })
	return st, idx, drained
}

func storedIndexedSHA(t *testing.T, st *store_sqlite.Store, prefix string) string {
	t.Helper()
	state, found, err := st.GetRepoIndexState(prefix)
	require.NoError(t, err)
	require.True(t, found, "repo_index_state row must exist")
	return state.IndexedSHA
}

// linkedWorktreeOnBranch adds a linked worktree checked out on a NEW branch.
// The branch is the whole point: `git worktree add --detach` leaves HEAD
// holding a raw SHA that git rewrites on every commit, so a detached fixture
// exercises the watched HEAD file and passes even with every ref watch missing.
func linkedWorktreeOnBranch(t *testing.T, repoDir, branch string) string {
	t.Helper()
	wtDir := filepath.Join(t.TempDir(), "wt")
	runGit(t, repoDir, "worktree", "add", "-q", "-b", branch, wtDir, "main")
	info, err := os.Stat(filepath.Join(wtDir, ".git"))
	require.NoError(t, err)
	require.False(t, info.IsDir(), "fixture must produce a linked worktree, not a nested .git dir")
	return wtDir
}

// disableReflog removes the HEAD reflog and stops git writing it again, so the
// refs-side watches are the only thing left that can carry a commit.
func disableReflog(t *testing.T, wtDir string) {
	t.Helper()
	runGit(t, wtDir, "config", "core.logAllRefUpdates", "false")
	gitDir, err := resolveGitDir(wtDir)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(filepath.Join(gitDir, "logs")))
}

func commitFile(t *testing.T, dir, file, fn string) string {
	t.Helper()
	writeFile(t, filepath.Join(dir, file), "package main\nfunc "+fn+"() {}\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", "commit "+fn)
	return headSHA(t, dir)
}

// TestGitWatcher_LinkedWorktreeCommitOnBranchRestamps is the regression test.
// It must fail before the watch set spans the common dir and the HEAD reflog.
func TestGitWatcher_LinkedWorktreeCommitOnBranchRestamps(t *testing.T) {
	repoDir := gitWatcherFixture(t)
	wtDir := linkedWorktreeOnBranch(t, repoDir, "wt-branch")
	st, idx, drained := startWatchedStoreIndex(t, wtDir, wtDir)
	before := storedIndexedSHA(t, st, idx.repoPrefix)

	head := commitFile(t, wtDir, "d.go", "Delta")
	require.NotEqual(t, before, head, "the fixture must move HEAD")
	awaitReconcile(t, drained, "the commit on the worktree's branch")

	assert.Equal(t, head, storedIndexedSHA(t, st, idx.repoPrefix),
		"a commit on a linked worktree's own branch must restamp indexed_sha")
}

// TestGitWatcher_SlashNamedBranchRestamps covers the branch names git stores
// one directory deeper: refs/heads/feat/foo fires nothing on refs/heads.
func TestGitWatcher_SlashNamedBranchRestamps(t *testing.T) {
	repoDir := gitWatcherFixture(t)
	wtDir := linkedWorktreeOnBranch(t, repoDir, "feat/foo")
	st, idx, drained := startWatchedStoreIndex(t, wtDir, wtDir)

	head := commitFile(t, wtDir, "d.go", "Delta")
	awaitReconcile(t, drained, "the commit on a slash-named branch")

	assert.Equal(t, head, storedIndexedSHA(t, st, idx.repoPrefix),
		"a branch name containing a slash must still restamp indexed_sha")
}

// TestGitWatcher_ReflogDisabledStillRestamps isolates the refs-side watches by
// removing the HEAD reflog entirely.
func TestGitWatcher_ReflogDisabledStillRestamps(t *testing.T) {
	repoDir := gitWatcherFixture(t)
	wtDir := linkedWorktreeOnBranch(t, repoDir, "no-reflog")
	disableReflog(t, wtDir)
	st, idx, drained := startWatchedStoreIndex(t, wtDir, wtDir)

	head := commitFile(t, wtDir, "d.go", "Delta")
	awaitReconcile(t, drained, "the commit with reflogs disabled")

	assert.Equal(t, head, storedIndexedSHA(t, st, idx.repoPrefix),
		"the common-dir refs watch must carry a commit on its own")
}

// TestGitWatcher_SlashBranchWithReflogDisabledRestamps is the case where both
// mechanisms would fail without the recursive walk of refs/heads: the reflog
// is gone and the branch ref lives one directory below the watched parent.
// It is what keeps the "two independent triggers" claim honest.
func TestGitWatcher_SlashBranchWithReflogDisabledRestamps(t *testing.T) {
	repoDir := gitWatcherFixture(t)
	wtDir := linkedWorktreeOnBranch(t, repoDir, "feat/no-reflog")
	disableReflog(t, wtDir)
	st, idx, drained := startWatchedStoreIndex(t, wtDir, wtDir)

	head := commitFile(t, wtDir, "d.go", "Delta")
	awaitReconcile(t, drained, "the commit on a slash branch with reflogs disabled")

	assert.Equal(t, head, storedIndexedSHA(t, st, idx.repoPrefix),
		"walking refs/heads subdirectories must carry the commit when the reflog cannot")
}

// TestGitWatcher_SiblingWatchersDoNotRestampEachOther pins the fan-out the
// shared common dir introduces: with N checkouts one commit wakes N watchers,
// and N-1 must return at reconcile's oldSHA == newSHA guard without restamping.
func TestGitWatcher_SiblingWatchersDoNotRestampEachOther(t *testing.T) {
	repoDir := gitWatcherFixture(t)
	wtA := linkedWorktreeOnBranch(t, repoDir, "sibling-a")
	wtB := linkedWorktreeOnBranch(t, repoDir, "sibling-b")
	stA, idxA, drainedA := startWatchedStoreIndex(t, wtA, wtA)
	stB, idxB, _ := startWatchedStoreIndex(t, wtB, wtB)
	beforeB := storedIndexedSHA(t, stB, idxB.repoPrefix)

	head := commitFile(t, wtA, "d.go", "Delta")
	awaitReconcile(t, drainedA, "the sibling's commit")
	assert.Equal(t, head, storedIndexedSHA(t, stA, idxA.repoPrefix),
		"the committing checkout must restamp")

	assert.Never(t, func() bool {
		return storedIndexedSHA(t, stB, idxB.repoPrefix) != beforeB
	}, 750*time.Millisecond, 50*time.Millisecond,
		"a sibling checkout must not restamp on another checkout's ref event")
}

// TestGitWatcher_PlainRepoWatchSetUnchanged is the negative control: the
// de-duplication must not drop refs/heads for an ordinary repository, whose
// gitdir and common dir are the same directory.
func TestGitWatcher_PlainRepoWatchSetUnchanged(t *testing.T) {
	repoDir := gitWatcherFixture(t)
	gitDir, err := resolveGitDir(repoDir)
	require.NoError(t, err)

	gw, err := NewGitWatcher(repoDir, nil, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, gw.Start())
	t.Cleanup(func() { _ = gw.Stop() })

	watched := make(map[string]struct{})
	for _, p := range gw.fsw.WatchList() {
		watched[p] = struct{}{}
	}
	_, ok := watched[filepath.Join(gitDir, "HEAD")]
	assert.True(t, ok, "a plain repo must still watch HEAD")

	// The ref-side subscription is the ACTIVE REF FILE, not the refs/heads
	// directory this test originally named. The watcher resolves HEAD's symref
	// and subscribes to exactly that path, which is strictly narrower and fixes
	// the case the directory watch never covered: a branch like feat/foo lives
	// in refs/heads/feat, so a non-recursive watch on refs/heads saw nothing.
	// The property under test is unchanged — a plain repo must still watch its
	// ref state — so the assertion follows the implementation rather than
	// pinning a path the watcher deliberately stopped using.
	// Matched on the refs/heads/ SUFFIX, not on an absolute path built from
	// gitDir. The ref watch is registered under the COMMON dir, which
	// resolveGitCommonDir obtains from `git rev-parse` with symlinks already
	// resolved; resolveGitDir does not resolve them. On macOS that alone is the
	// difference between /var/folders/... and /private/var/folders/..., so an
	// absolute-prefix comparison fails on a watch that is genuinely installed.
	refsHeads := filepath.Join("refs", "heads") + string(filepath.Separator)
	var activeRef string
	for path := range watched {
		if strings.Contains(path, refsHeads) {
			activeRef = path
			break
		}
	}
	assert.NotEmptyf(t, activeRef,
		"a plain repo must watch its active ref under refs/heads, watched=%v", watched)

	st, idx, drained := startWatchedStoreIndex(t, repoDir, repoDir)
	head := commitFile(t, repoDir, "d.go", "Delta")
	awaitReconcile(t, drained, "a commit in a plain repo")
	assert.Equal(t, head, storedIndexedSHA(t, st, idx.repoPrefix),
		"a plain repo must still restamp on a commit")
}

// TestGitWatcher_RefusesADegenerateWatchSet pins the external signal that this
// class of bug produces.
//
// The signal used to be a Warn line and a watcher that started anyway, blind.
// It is now a REFUSAL: Start returns an error rather than publishing a watcher
// that silently missed its ref state. That is strictly stronger, and the
// operator-visible half is unchanged because MultiWatcher logs the failed
// Start — see TestMultiWatcherWarnsWhenTheGitWatcherCannotStart, which covers
// the case that actually occurred in the field.
func TestGitWatcher_RefusesADegenerateWatchSet(t *testing.T) {
	t.Run("refuses when nothing ref-side is watchable", func(t *testing.T) {
		repoDir := t.TempDir()
		gitDir := filepath.Join(repoDir, ".git")
		require.NoError(t, os.MkdirAll(gitDir, 0o755))
		writeFile(t, filepath.Join(gitDir, "HEAD"), "ref: refs/heads/main\n")

		core, _ := observer.New(zapcore.WarnLevel)
		gw, err := NewGitWatcher(repoDir, nil, zap.New(core))
		require.NoError(t, err)
		t.Cleanup(func() { _ = gw.Stop() })

		// A .git directory holding nothing but a HEAD file is not a repository,
		// so the common-dir resolution refuses it before any watch is added.
		err = gw.Start()
		require.Error(t, err, "a watcher with no reachable ref state must not start")
		assert.Empty(t, gw.fsw.WatchList(),
			"a refused Start must not leave subscriptions behind")
	})

	t.Run("stays quiet on a healthy repo", func(t *testing.T) {
		repoDir := gitWatcherFixture(t)
		core, logs := observer.New(zapcore.WarnLevel)
		gw, err := NewGitWatcher(repoDir, nil, zap.New(core))
		require.NoError(t, err)
		require.NoError(t, gw.Start())
		t.Cleanup(func() { _ = gw.Stop() })

		assert.Zero(t, logs.FilterMessageSnippet("no ref watch installed").Len(),
			"a healthy warm restart must not warn")
	})
}

// TestGitCommonDir_LayoutTable covers the three layouts Start has to resolve.
// The third row is the one the ResolveWorktree reuse makes load-bearing: a
// submodule main checkout has no commondir file, so GitCommonDir comes back
// empty and only the fallback keeps its refs watched.
func TestGitCommonDir_LayoutTable(t *testing.T) {
	repoDir := gitWatcherFixture(t)

	t.Run("plain repo resolves to its own .git", func(t *testing.T) {
		gitDir, err := resolveGitDir(repoDir)
		require.NoError(t, err)
		assert.Equal(t, gitDir, gitCommonDir(repoDir, gitDir))
	})

	t.Run("linked worktree resolves to the common dir", func(t *testing.T) {
		wtDir := linkedWorktreeOnBranch(t, repoDir, "common-dir-probe")
		gitDir, err := resolveGitDir(wtDir)
		require.NoError(t, err)

		raw, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
		require.NoError(t, err)
		require.False(t, filepath.IsAbs(strings.TrimSpace(string(raw))),
			"git writes commondir relative; an absolute fixture would not exercise the join")

		// EvalSymlinks because git writes the resolved path into the gitdir
		// file, and on macOS t.TempDir() hands back /var/... for /private/var.
		want, err := filepath.EvalSymlinks(filepath.Join(repoDir, ".git"))
		require.NoError(t, err)
		assert.Equal(t, want, gitCommonDir(wtDir, gitDir))
	})

	t.Run("submodule main checkout falls back to its own gitdir", func(t *testing.T) {
		subDir := t.TempDir()
		sepGitDir := filepath.Join(t.TempDir(), "modules", "sub")
		require.NoError(t, os.MkdirAll(filepath.Dir(sepGitDir), 0o755))
		runGit(t, subDir, "init", "-q", "-b", "main", "--separate-git-dir", sepGitDir)

		gitDir, err := resolveGitDir(subDir)
		require.NoError(t, err)
		require.NoFileExists(t, filepath.Join(gitDir, "commondir"),
			"the fixture must reproduce the submodule shape: a gitdir file with no commondir")

		assert.Equal(t, gitDir, gitCommonDir(subDir, gitDir),
			"a submodule main checkout must keep watching its own gitdir, not fall through to empty")
	})
}
