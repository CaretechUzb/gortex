package indexer

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/search"
)

// freshnessFixture builds a git repo on a durable store — the in-memory
// graph is not a RepoIndexStateWriter, so freshness is never persisted
// there — indexes it once, and hands back everything a reconcile needs.
func freshnessFixture(t *testing.T) (repoPath string, cm *config.ConfigManager, store *store_sqlite.Store, prior map[string]int64) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}

	dir := t.TempDir()
	repoPath = filepath.Join(dir, "repo")
	require.NoError(t, exec.Command("mkdir", "-p", repoPath).Run())
	runGit(t, repoPath, "init", "-q", "-b", "main")
	runGit(t, repoPath, "config", "user.email", "test@example.com")
	runGit(t, repoPath, "config", "user.name", "Test")
	runGit(t, repoPath, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(repoPath, "a.go"), "package main\n\nfunc Alpha() {}\n")
	runGit(t, repoPath, "add", ".")
	runGit(t, repoPath, "commit", "-q", "-m", "first")

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	store, err = store_sqlite.Open(filepath.Join(t.TempDir(), "store.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	mi := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	return repoPath, cm, store, mi.FileMtimes("repo")
}

func indexedSHA(t *testing.T, store *store_sqlite.Store) string {
	t.Helper()
	reader, ok := graph.Store(store).(graph.RepoIndexStateReader)
	require.True(t, ok)
	st, found, err := reader.GetRepoIndexState("repo")
	require.NoError(t, err)
	require.True(t, found, "a full index must have persisted a freshness row")
	return st.IndexedSHA
}

// The regression, and the exact shape the live workspace hit: `git commit`
// does not touch working-tree file mtimes, so a commit made while the
// daemon is down leaves every file looking already-indexed. The reconcile
// correctly finds nothing to do — and the freshness row keeps the SHA from
// the last full index, so `gortex repos` reports the repo stale forever
// while its graph is exactly current. Only the git watcher restamped that
// row, and the watcher cannot see a transition that happened while the
// daemon was not running.
func TestReconcileRepoCtx_RestampsFreshnessAfterCommitMadeWhileDown(t *testing.T) {
	repoPath, cm, store, prior := freshnessFixture(t)
	firstSHA := indexedSHA(t, store)

	// A commit that changes nothing on disk: HEAD moves, mtimes do not.
	runGit(t, repoPath, "commit", "-q", "--allow-empty", "-m", "while the daemon was down")
	secondSHA := gitHead(t, repoPath)
	require.NotEqual(t, firstSHA, secondSHA)

	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	result, err := restarted.ReconcileRepoCtx(context.Background(),
		config.RepoEntry{Path: repoPath, Name: "repo"}, prior)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.FullRetrack,
		"nothing changed on disk, so this must not be a full re-track — a full re-track would stamp the row on its own and prove nothing")

	assert.Equal(t, secondSHA, indexedSHA(t, store),
		"a reconcile that proves the graph current must stamp the row at HEAD")
}

// Same restamp on the route that does reindex files, so the fix is not
// specific to the no-op case.
func TestReconcileRepoCtx_RestampsFreshnessAfterCommittedEdit(t *testing.T) {
	repoPath, cm, store, prior := freshnessFixture(t)
	firstSHA := indexedSHA(t, store)

	writeFile(t, filepath.Join(repoPath, "a.go"), "package main\n\nfunc Alpha() {}\n\nfunc Gamma() {}\n")
	runGit(t, repoPath, "add", ".")
	runGit(t, repoPath, "commit", "-q", "-m", "edit while down")
	secondSHA := gitHead(t, repoPath)
	require.NotEqual(t, firstSHA, secondSHA)

	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err := restarted.ReconcileRepoCtx(context.Background(),
		config.RepoEntry{Path: repoPath, Name: "repo"}, prior)
	require.NoError(t, err)

	assert.Equal(t, secondSHA, indexedSHA(t, store))
}

// The restamp must not fire when HEAD has not moved: the probe exists to
// close a gap, not to shell out to git on every reconcile of every repo.
func TestReconcileRepoCtx_LeavesFreshnessAloneWhenHeadUnchanged(t *testing.T) {
	repoPath, cm, store, prior := freshnessFixture(t)
	firstSHA := indexedSHA(t, store)

	restarted := NewMultiIndexer(graph.Store(store), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err := restarted.ReconcileRepoCtx(context.Background(),
		config.RepoEntry{Path: repoPath, Name: "repo"}, prior)
	require.NoError(t, err)

	assert.Equal(t, firstSHA, indexedSHA(t, store))
}
