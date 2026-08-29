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
