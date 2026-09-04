package indexer

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/search"
)

// copySourceRepo is a git repository with enough files that committing one more
// into a worktree is an ordinary edit rather than wholesale churn.
//
// The size is load-bearing, not decoration: ReconcileRepoCtx routes to a full
// retrack once churn exceeds 40% of the prior file count, and a full retrack
// runs its own pipeline and has no tail to defer. A one-file repository plus one
// new file is 100% churn, which is the copy path's fallback shape rather than
// the one under test here.
func copySourceRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git binary not available in PATH")
	}
	repo := filepath.Join(t.TempDir(), "repo")
	gitInitRepo(t, repo)
	for i := 0; i < 12; i++ {
		writeFile(t, filepath.Join(repo, fmt.Sprintf("pkg%d.go", i)),
			fmt.Sprintf("package main\n\nfunc Fn%d() {}\n", i))
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-q", "-m", "init")
	return repo
}

// copyTrackHarness is the sqlite-backed, real-git setup the worktree-copy path
// actually needs: graph.Graph implements neither CopyRepoSubgraph nor
// RepoIndexStateReader, so the unit fixtures in track_worktree_copy_test.go stop
// at worktreeCopySource and never reach trackWorktreeByCopy at all.
//
// Returns the MultiIndexer with the source repository tracked, the source's
// path, and the observed log.
func copyTrackHarness(t *testing.T) (*MultiIndexer, string, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.InfoLevel)
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	mi := NewMultiIndexer(openTestSqlite(t), reg, search.NewNull(), newTestConfigManager(t), zap.New(core))
	t.Cleanup(mi.stopWorkspaceRederive)

	// The SOURCE repository is tracked inside a batch, then released. Tracked
	// outside one it would legitimately schedule its own workspace derivation,
	// and a multi-second pass running underneath every assertion below is noise
	// this test has no way to tell from the behaviour it is measuring. Inside a
	// batch the track only records that it is owed, and clearing the record
	// leaves the scheduler genuinely idle — which is the baseline the worktree
	// assertions are read against.
	mi.BeginParallelBatch()
	repo := copySourceRepo(t)
	res, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repo, Name: "src"})
	require.NoError(t, err)
	require.NotNil(t, res, "the source repository must actually be tracked")
	mi.ResetBatch()
	mi.ClearDeferredWorkspaceRederive()
	require.False(t, mi.WorkspaceRederivePending(), "the harness must start with an idle scheduler")
	return mi, repo, logs
}

// divergedWorktree checks out a linked worktree of repo and commits one extra
// file into it, so the copy source's indexed commit and the worktree's HEAD
// really differ and the copy takes its reconcile path.
func divergedWorktree(t *testing.T, repo, branch string) string {
	t.Helper()
	wt := addWorktree(t, repo, branch)
	writeFile(t, filepath.Join(wt, "extra.go"), "package main\n\nfunc Extra() {}\n")
	runGit(t, wt, "add", ".")
	runGit(t, wt, "commit", "-q", "-m", "diverge")
	return wt
}

// trackDivergedCopy tracks wt and asserts it really went through the copy path
// rather than a cold index, which is the precondition every assertion below
// depends on.
func trackDivergedCopy(t *testing.T, mi *MultiIndexer, wt string, logs *observer.ObservedLogs) *IndexResult {
	t.Helper()
	res, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: wt, AsWorktree: true})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.NotEmpty(t, res.RepoPrefix)
	require.Len(t, logs.FilterMessage("worktree installed by subgraph copy").All(), 1,
		"the track must have taken the copy path, not a cold index")
	require.False(t, res.FullRetrack,
		"the reconcile must have stayed scoped; a full retrack has no tail to defer")
	return res
}

// rederiveState reports what the scheduler owes prefix: queued for a repo-wide
// pass (pending or already taken by a running one), and deferred behind a batch.
// Read per-prefix rather than through WorkspaceRederivePending, which is a
// workspace-wide bool and would also see the source repository's own pass.
func rederiveState(mi *MultiIndexer, prefix string) (queued, deferred bool) {
	mi.rederive.mu.Lock()
	defer mi.rederive.mu.Unlock()
	_, inPending := mi.rederive.pending[prefix]
	_, inFlight := mi.rederive.inflight[prefix]
	_, inDeferred := mi.rederive.deferred[prefix]
	return inPending || inFlight, inDeferred
}

// TestCopyTrackDuringABatchDefersItsTailInsteadOfDerivingTheWorkspace is the
// regression gate for the 54-minute track.
//
// A diverged copy whose reconcile tail was suppressed by an open batch used to
// be indistinguishable from one whose reconcile genuinely re-indexed the world:
// copiedDivergenceRepaired reports only that the tail did not run, so both fell
// through to scheduleWorkspaceRederive — a repo-wide pass that re-derives edges
// the copy already carried. Measured 2026-09-02 on a 192-file divergence tracked
// during daemon warmup: 3,255s, against 27.8s for the reconcile itself.
func TestCopyTrackDuringABatchDefersItsTailInsteadOfDerivingTheWorkspace(t *testing.T) {
	mi, repo, logs := copyTrackHarness(t)
	wt := divergedWorktree(t, repo, "feature")

	mi.BeginParallelBatch()
	res := trackDivergedCopy(t, mi, wt, logs)
	prefix := res.RepoPrefix

	// The tail is held, not discarded.
	mi.mu.RLock()
	_, held := mi.deferredCopyTails[prefix]
	mi.mu.RUnlock()
	require.True(t, held, "the batch-suppressed tail must be recorded for replay")

	// And no repo-wide derivation was scheduled for it.
	queued, owed := rederiveState(mi, prefix)
	require.False(t, queued, "a batch-suppressed copy must not schedule the repo-wide rederive")
	require.True(t, owed, "the repo must still read as owed until its tail replays")

	// The batch transition replays it.
	mi.ResetBatch()
	require.Empty(t, mi.FlushDeferredWorkspaceRederive(),
		"a replayed tail leaves nothing for the repo-wide fallback to schedule")

	require.Len(t,
		logs.FilterMessage("worktree copy: divergence repaired by the deferred reconcile tail; no workspace rederive owed").All(), 1,
		"the replay must report the divergence repaired")
	require.True(t, res.DerivedTailRan, "the replayed tail must mark the result derived")

	mi.mu.RLock()
	remaining := len(mi.deferredCopyTails)
	mi.mu.RUnlock()
	require.Zero(t, remaining, "a replayed tail must not be replayed again at the next transition")

	queued, owed = rederiveState(mi, prefix)
	require.False(t, queued, "the replay must not fall back to the repo-wide pass")
	require.False(t, owed, "a repaired repo must leave the owed set")
}

// TestCopyTrackOutsideABatchStillRepairsInline guards the path this change must
// not disturb: with no batch suppressing it, the reconcile runs its own scoped
// tail and the copy is square before trackWorktreeByCopy returns. Nothing is
// deferred and nothing is scheduled.
func TestCopyTrackOutsideABatchStillRepairsInline(t *testing.T) {
	mi, repo, logs := copyTrackHarness(t)
	wt := divergedWorktree(t, repo, "inline")

	res := trackDivergedCopy(t, mi, wt, logs)
	require.True(t, res.DerivedTailRan)
	require.Len(t,
		logs.FilterMessage("worktree copy: divergence repaired by the reconcile's scoped tail; no workspace rederive owed").All(), 1,
		"an unbatched copy must still repair inline")

	mi.mu.RLock()
	deferred := len(mi.deferredCopyTails)
	mi.mu.RUnlock()
	require.Zero(t, deferred, "nothing is owed when the inline tail already ran")

	queued, owed := rederiveState(mi, res.RepoPrefix)
	require.False(t, queued, "an inline repair must not schedule the repo-wide pass")
	require.False(t, owed)
}
