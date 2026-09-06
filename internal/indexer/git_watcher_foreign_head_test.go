package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// startWorktreeWatcher indexes the linked worktree and starts a GitWatcher on
// it, returning both so a test can drive reconcile directly.
func startWorktreeWatcher(t *testing.T, fam foreignHeadFamily) (*graph.Graph, *GitWatcher) {
	t.Helper()

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(fam.worktree)
	_, err := idx.IndexCtx(testCtx(), fam.worktree)
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("SharedFunc"),
		"the worktree's own source must be indexed before the reconcile")

	gw, err := NewGitWatcher(fam.worktree, idx, zap.NewNop())
	require.NoError(t, err)
	gw.debounce = 50 * time.Millisecond
	require.NoError(t, gw.Start())
	t.Cleanup(func() { _ = gw.Stop() })

	gw.mu.Lock()
	seeded := gw.lastSHA
	gw.mu.Unlock()
	require.Equal(t, fam.sharedSHA, seeded, "watcher must seed at the worktree's own commit")

	return g, gw
}

func watcherLastSHA(gw *GitWatcher) string {
	gw.mu.Lock()
	defer gw.mu.Unlock()
	return gw.lastSHA
}

// TestGitWatcher_RefusesForeignRepositoryHeadDuringWorktreeRemoval is the
// regression for the 2026-09-04 `local@MR6410` incident: a ref event landing
// while a linked worktree was being removed reconciled the worktree's graph
// against the SUPERPROJECT's HEAD and deleted 2,845 files.
//
// `git worktree remove` and `rm -rf <worktree>` are both non-atomic. Between
// the moment the `.git` link is unlinked and the moment the directory itself
// disappears, the worktree root is a plain directory — and `git -C <root>
// rev-parse HEAD` does not fail there, it walks UP and answers for the
// enclosing repository. Because the two repositories share history the
// follow-up diff succeeds as well, so nothing in the pipeline notices that the
// commit belongs to somebody else.
//
// Without the identity guard this test fails twice over: lastSHA advances to
// the superproject's commit and the worktree's own symbol is evicted from the
// graph by a diff that was computed in another repository.
func TestGitWatcher_RefusesForeignRepositoryHeadDuringWorktreeRemoval(t *testing.T) {
	fam := newForeignHeadFamily(t)
	g, gw := startWorktreeWatcher(t, fam)

	// The removal window: the `.git` link is already unlinked and the source
	// files are going away, but the root itself is still on disk.
	require.NoError(t, os.Remove(filepath.Join(fam.worktree, ".git")))
	require.NoError(t, os.Remove(filepath.Join(fam.worktree, "shared.go")))

	// Anchor the premise with a direct git call: from this root, git now
	// answers with the superproject's HEAD and reports no error at all.
	require.Equal(t, fam.superSHA, gitHead(t, fam.worktree),
		"premise: a de-linked worktree root resolves upward to the enclosing repository")

	gw.reconcile("packed-refs")

	assert.Equal(t, fam.sharedSHA, watcherLastSHA(gw),
		"a commit from another repository must never become this checkout's baseline")
	assert.NotEmpty(t, g.FindNodesByName("SharedFunc"),
		"a foreign diff must never evict this checkout's files; that is the vanished-checkout sweep's job")
}

// TestGitWatcher_RefusesReconcileWhenCheckoutRootGone pins the other half of
// the fail-closed contract: once the root is gone the checkout is MISSING and
// belongs to the checkout-lifecycle sweep, which applies the availability and
// removal graces. A ref reconcile must not be the thing that empties it.
func TestGitWatcher_RefusesReconcileWhenCheckoutRootGone(t *testing.T) {
	fam := newForeignHeadFamily(t)
	g, gw := startWorktreeWatcher(t, fam)

	require.NoError(t, os.RemoveAll(fam.worktree))

	gw.reconcile("packed-refs")

	assert.Equal(t, fam.sharedSHA, watcherLastSHA(gw),
		"a vanished checkout must not advance its commit baseline")
	assert.NotEmpty(t, g.FindNodesByName("SharedFunc"),
		"a vanished checkout must keep its graph until the lifecycle sweep retires it")
}
