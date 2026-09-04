package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// These two log lines are not decoration, and that is why they are asserted.
//
// `trigger` is the field that says WHICH caller drove a reconcile — an fsnotify
// delivery names the path it observed, a programmatic call names itself. Its
// absence is what made a reconcile that was still in flight indistinguishable
// from a watcher that never fired, and produced a root-cause diagnosis that had
// to be retracted in full.
//
// The git-watcher start failure was logged at Debug, which is off by default. A
// repository with no git watcher has no path that restamps freshness after a
// checkout, so it reads stale for the life of the daemon; silence made that
// indistinguishable from a watcher that was simply quiet. Raising it to Warn is
// the whole fix, so the assertion has to be level-sensitive — an observer
// admitting only Warn and above fails if either line slips back to Debug.

// gitWatcherWithObservedLog indexes repoDir and attaches a started GitWatcher
// whose entries the test can read back. It mirrors startWatchedIndex, which
// hardcodes a no-op logger and so cannot see any of this.
func gitWatcherWithObservedLog(t *testing.T, repoDir string) (*GitWatcher, *Indexer, *observer.ObservedLogs, <-chan int) {
	t.Helper()

	g := graph.New()
	idx := New(g, newTestRegistry(), config.IndexConfig{Workers: 1}, zap.NewNop())
	idx.search = search.NewNull()
	idx.SetRootPath(repoDir)
	_, err := idx.IndexCtx(testCtx(), repoDir)
	require.NoError(t, err)

	core, logs := observer.New(zapcore.InfoLevel)
	gw, err := NewGitWatcher(repoDir, idx, zap.New(core))
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
	return gw, idx, logs, drained
}

// lastReconcileTrigger returns the `trigger` field of the most recent
// reconcile line, failing if the line is absent or carries no such field.
func lastReconcileTrigger(t *testing.T, logs *observer.ObservedLogs) string {
	t.Helper()
	entries := logs.FilterMessage("git-watcher: reconciled ref change").All()
	require.NotEmpty(t, entries, "a completed reconcile must log 'git-watcher: reconciled ref change'")

	fields := entries[len(entries)-1].ContextMap()
	trigger, ok := fields["trigger"].(string)
	require.True(t, ok, "the reconcile line must carry a string `trigger` field, got fields %v", fields)
	require.NotEmpty(t, trigger, "`trigger` must name its caller, not be blank")
	return trigger
}

// TestGitWatcherReconcileLogNamesItsTrigger pins the field in BOTH directions,
// because either half alone is satisfiable by a constant.
//
// A watcher-driven reconcile must name the path it observed, and a programmatic
// one must name itself. Asserting only the first would pass against a
// hardcoded string; asserting only the second would pass against a field that
// merely echoes its argument and is never set by the event path.
func TestGitWatcherReconcileLogNamesItsTrigger(t *testing.T) {
	repoDir := gitWatcherFixture(t)
	// The started watcher is driven through git commands rather than through
	// its handle, so only its logs and drain signal are named here.
	_, idx, logs, drained := gitWatcherWithObservedLog(t, repoDir)

	// Half one: the fsnotify path. The watcher names the file it saw change,
	// which under a normal checkout is the repository's own HEAD.
	runGit(t, repoDir, "checkout", "-q", "feature-one")
	awaitReconcile(t, drained, "checkout to feature-one")

	fromEvent := lastReconcileTrigger(t, logs)
	assert.Contains(t, fromEvent, "HEAD",
		"an fsnotify-driven reconcile must name the watched path it observed, got %q", fromEvent)

	// Half two: the programmatic path, on a SECOND watcher that is never
	// started.
	//
	// This half used to stop `gw` first, so that no fsnotify event could race
	// the direct call. That stopped working when reconcile gained its
	// stopCalled guard: a stopped watcher returns immediately and logs nothing,
	// so the assertion below read half one's entry and compared a path against
	// "startup-catchup". Leaving `gw` running is not an option either — a
	// concurrent watcher-driven reconcile coalesces the direct one into a
	// `rerun` flag, which also logs nothing under this trigger.
	//
	// An unstarted watcher has neither problem: no event source, no in-flight
	// run, and stopCalled false. It is also the exact shape the startup
	// catch-up runs in, which is what this trigger name belongs to.
	core, directLogs := observer.New(zapcore.InfoLevel)
	direct, err := NewGitWatcher(repoDir, idx, zap.New(core))
	require.NoError(t, err)

	// Seed the baseline before moving HEAD, exactly as Start's catch-up does.
	// Without it lastSHA is empty, reconcile takes its first-observation path,
	// and it returns before logging anything at all.
	baseline, err := direct.currentSHA(testCtx())
	require.NoError(t, err)
	require.NotEmpty(t, baseline)
	direct.mu.Lock()
	direct.lastSHA = baseline
	direct.mu.Unlock()

	runGit(t, repoDir, "checkout", "-q", "feature-two")
	direct.reconcile("startup-catchup")

	assert.Equal(t, "startup-catchup", lastReconcileTrigger(t, directLogs),
		"a programmatic reconcile must report its own caller, not the watched path")
}

// TestMultiWatcherWarnsWhenTheGitWatcherCannotStart covers the case that
// actually occurred: a tracked directory with no .git of any kind. In the live
// workspace that is `addons`, which is tracked as its own repository but is
// physically a subdirectory of another repository's working tree — so it has
// 17,466 files that can never restamp themselves, and nobody knew until this
// line was raised to Warn.
//
// The observer admits Warn and above only, so this fails if the line returns to
// Debug even though the string is unchanged.
func TestMultiWatcherWarnsWhenTheGitWatcherCannotStart(t *testing.T) {
	repoDir := filepath.Join(t.TempDir(), "no-git-repo")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	writeFile(t, filepath.Join(repoDir, "main.go"), "package main\n\nfunc Hello() {}\n")

	cm := newTestConfigManager(t)
	cm.Global().Repos = []config.RepoEntry{{Path: repoDir, Name: "no-git"}}

	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err := mi.IndexAll()
	require.NoError(t, err)

	core, logs := observer.New(zapcore.WarnLevel)
	mw, err := NewMultiWatcher(mi, map[string]config.WatchConfig{}, zap.New(core))
	require.NoError(t, err)
	t.Cleanup(func() { _ = mw.Stop() })

	// AddRepo must still succeed: a missing git watcher degrades ref
	// reconciliation, it does not make the repository unwatchable.
	require.NoError(t, mw.AddRepo("no-git", config.WatchConfig{Enabled: true, DebounceMs: 50}))

	// Keyed on the START branch specifically, not on the message half both
	// branches share. An earlier draft accepted either, and a mutation run
	// showed what that cost: reverting the sibling `init failed` line to Debug
	// left this test green, so it was guarding neither line in particular.
	//
	// Start is also the only branch reachable from a test. NewGitWatcher fails
	// only on filepath.Abs or fsnotify handle exhaustion; the missing .git is
	// discovered by Start's stat, which is exactly the shape the live workspace
	// produced. The `init failed` line is therefore left unguarded on purpose
	// rather than by oversight.
	warned := logs.FilterMessage("git-watcher: start failed; ref changes will not be reconciled").All()
	require.NotEmpty(t, warned,
		"a repository whose git watcher cannot start must say so at Warn, not Debug; entries seen: %v",
		logs.All())
	require.Equal(t, zapcore.WarnLevel, warned[0].Level)
	assert.Equal(t, "no-git", warned[0].ContextMap()["prefix"],
		"the warning must name the repository that will not reconcile")
}
