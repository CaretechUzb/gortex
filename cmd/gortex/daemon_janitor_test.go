package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/runtimeactivity"
	"github.com/zzet/gortex/internal/search"
)

// janitorTestRepo builds a single-repo workspace, indexes it once (the
// "previous daemon run" that recorded the mtimes), and hands back the live
// graph plus the MultiIndexer a janitor would sweep. extraFiles pads the
// repo so a sweep costs enough wall time to be observable — see
// TestASecondKickDuringASweepDoesNotQueueUnboundedly.
func janitorTestRepo(t *testing.T, extraFiles int) (*graph.Graph, *indexer.MultiIndexer, string) {
	t.Helper()

	dir := t.TempDir()
	repoPath := filepath.Join(dir, "repo")
	require.NoError(t, os.MkdirAll(repoPath, 0o755))
	writeJanitorFile(t, filepath.Join(repoPath, "base.go"), "package main\n\nfunc Base() {}\n")
	for i := 0; i < extraFiles; i++ {
		writeJanitorFile(t, filepath.Join(repoPath, fmt.Sprintf("pad%03d.go", i)),
			fmt.Sprintf("package main\n\nfunc Pad%03d() {}\n", i))
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: repoPath, Name: "repo"}}}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	g := graph.New()
	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	mi := indexer.NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	return g, mi, repoPath
}

// janitorTestIndexerNoRepos builds a MultiIndexer over an empty workspace —
// the shape the janitor sees if a sweep ever fires before warmup has
// registered anything.
func janitorTestIndexerNoRepos(t *testing.T) *indexer.MultiIndexer {
	t.Helper()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)

	reg := parser.NewRegistry()
	languages.RegisterAll(reg)
	return indexer.NewMultiIndexer(graph.New(), reg, search.NewNull(), cm, zap.NewNop())
}

func writeJanitorFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
}

// janitorLogger returns a debug-level logger plus its observed log, so a
// test can count sweeps and tell a kick-driven sweep from a ticker-driven
// one. "janitor: sweep starting" is the only unconditional per-sweep line;
// "janitor: reconciled repo" fires only when something actually changed.
func janitorLogger() (*zap.Logger, *observer.ObservedLogs) {
	core, logs := observer.New(zap.DebugLevel)
	return zap.New(core), logs
}

func sweepTriggers(logs *observer.ObservedLogs) []string {
	entries := logs.FilterMessage("janitor: sweep starting").All()
	triggers := make([]string, 0, len(entries))
	for _, e := range entries {
		trigger, _ := e.ContextMap()["trigger"].(string)
		triggers = append(triggers, trigger)
	}
	return triggers
}

// TestTheReconcileJanitorSweepsOnceImmediatelyAfterWarmup is the regression
// for the watcher blind window. Everything a daemon misses between its
// warmup snapshot and the moment its watcher starts is invisible to both,
// and ReconcileRepoCtx is first-registration-only, so the janitor is the
// only thing that can pick it up — except time.NewTicker has no t=0 tick,
// so its first sweep landed one whole interval (1 h by default) after
// start, and every restart reset that clock.
//
// The 1 h interval is what makes this test mean anything: it is far longer
// than the test could ever wait, so a sweep observed here can only have
// come from sweepNow. Restore the plain ticker and this fails; a test that
// merely called ReconcileAll would stay green.
func TestTheReconcileJanitorSweepsOnceImmediatelyAfterWarmup(t *testing.T) {
	g, mi, repoPath := janitorTestRepo(t, 0)

	// The save the daemon was down for: a file that reached disk while
	// neither the snapshot nor the watcher was looking.
	writeJanitorFile(t, filepath.Join(repoPath, "saved_while_down.go"),
		"package main\n\nfunc SavedWhileDaemonWasDown() {}\n")
	require.Empty(t, g.GetFileNodes("repo/saved_while_down.go"),
		"the offline save must be missing from the graph before the sweep")

	logger, logs := janitorLogger()
	sweepNow, stop := startReconcileJanitor(mi, time.Hour, logger)
	defer stop()

	sweepNow()

	require.Eventually(t, func() bool {
		return len(g.GetFileNodes("repo/saved_while_down.go")) > 0
	}, 30*time.Second, 20*time.Millisecond,
		"the warmup kick must sweep immediately, not one interval later")

	assert.Equal(t, []string{"kick"}, sweepTriggers(logs),
		"exactly one sweep, and it came from the kick — a 1 h ticker cannot have fired")
}

// TestASecondKickDuringASweepDoesNotQueueUnboundedly pins the two
// properties of the kick channel that keep shutdown safe: it is buffered to
// 1, so a burst collapses into at most one follow-up sweep instead of
// queueing one sweep per caller, and the send is non-blocking, so a kick
// that arrives after stop is dropped rather than parking forever on a
// goroutine that has already returned. Make it a plain unbuffered channel
// and the post-stop half of this test hangs the deferred stop — which is
// exactly the failure this shape exists to prevent.
func TestASecondKickDuringASweepDoesNotQueueUnboundedly(t *testing.T) {
	// Pad the repo so one sweep costs enough wall time that the whole burst
	// below lands inside it; ten channel sends are microseconds.
	_, mi, _ := janitorTestRepo(t, 120)

	logger, logs := janitorLogger()
	sweepNow, stop := startReconcileJanitor(mi, time.Hour, logger)

	const burst = 10
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < burst; i++ {
			sweepNow()
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("sweepNow blocked: the kick send must be non-blocking")
	}

	// Let the janitor drain whatever it queued, then assert the count.
	require.Eventually(t, func() bool {
		return len(sweepTriggers(logs)) >= 1
	}, 30*time.Second, 20*time.Millisecond, "the burst must produce at least one sweep")
	// A second sweep, if the buffer held one kick, needs time to finish too.
	time.Sleep(500 * time.Millisecond)

	triggers := sweepTriggers(logs)
	assert.LessOrEqual(t, len(triggers), 2,
		"a buffer of 1 collapses a burst of %d kicks into at most one follow-up sweep, got %d",
		burst, len(triggers))
	for _, trigger := range triggers {
		assert.Equal(t, "kick", trigger, "no ticker can have fired on a 1 h interval")
	}

	// Now the shutdown half: stop the janitor, then keep kicking. With an
	// unbuffered channel and no receiver these sends never return and the
	// deferred stop in runDaemonStart would hang the daemon's shutdown.
	stop()
	postStop := make(chan struct{})
	go func() {
		defer close(postStop)
		for i := 0; i < burst; i++ {
			sweepNow()
		}
	}()
	select {
	case <-postStop:
	case <-time.After(10 * time.Second):
		t.Fatal("sweepNow blocked after stop: a kick that loses the shutdown race must be dropped")
	}
}

// TestTheJanitorStillHonoursGORTEX_RECONCILE_INTERVAL_off pins that the new
// immediate sweep does not resurrect a janitor the operator turned off.
// reconcileInterval returns 0 for "0" and "off", startReconcileJanitor
// disables itself on a non-positive interval, and the disabled path has to
// swallow the kick as well as the ticker.
func TestTheJanitorStillHonoursGORTEX_RECONCILE_INTERVAL_off(t *testing.T) {
	for _, raw := range []string{"0", "off"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("GORTEX_RECONCILE_INTERVAL", raw)
			require.Zero(t, reconcileInterval(),
				"%q must disable the janitor, not reinterpret it as a duration", raw)

			g, mi, repoPath := janitorTestRepo(t, 0)
			writeJanitorFile(t, filepath.Join(repoPath, "saved_while_down.go"),
				"package main\n\nfunc SavedWhileDaemonWasDown() {}\n")

			logger, logs := janitorLogger()
			sweepNow, stop := startReconcileJanitor(mi, reconcileInterval(), logger)
			defer stop()

			sweepNow()
			// Nothing to wait on: a disabled janitor has no goroutine. Give a
			// stray one a window to prove itself before asserting the absence.
			time.Sleep(300 * time.Millisecond)

			assert.Empty(t, sweepTriggers(logs),
				"a disabled janitor must swallow the kick, not sweep once and stop")
			assert.Empty(t, g.GetFileNodes("repo/saved_while_down.go"),
				"the operator turned reconciliation off; the kick must not reindex behind their back")
			assert.Len(t, logs.FilterMessage("daemon: reconcile janitor disabled").All(), 1,
				"the disabled decision stays visible in the log")
		})
	}
}

// TestTheFirstSweepRunsAfterTheIndexersAreRegistered covers the ordering the
// kick depends on. The janitor is started at daemon.go:484 — before
// srv.Listen and before warmup — so a sweep that fired at construction time
// would walk a MultiIndexer with nothing registered yet and silently do
// nothing while looking like it had swept. The kick is issued from the
// warmup goroutine instead, after warmupDaemonState returns, by which point
// every repo is registered and the watcher is live.
func TestTheFirstSweepRunsAfterTheIndexersAreRegistered(t *testing.T) {
	t.Run("a sweep with nothing registered is inert, not a crash", func(t *testing.T) {
		mi := janitorTestIndexerNoRepos(t)
		require.Empty(t, mi.AllMetadata(), "no repo is registered before warmup")

		logger, logs := janitorLogger()
		sweepNow, stop := startReconcileJanitor(mi, time.Hour, logger)
		defer stop()

		require.NotPanics(t, sweepNow)
		require.Eventually(t, func() bool {
			return len(sweepTriggers(logs)) == 1
		}, 30*time.Second, 20*time.Millisecond, "the kick still reaches the sweep body")

		// It ran, and it found nothing — which is the point. A sweep at this
		// moment cannot repair anything, so it must not report that it did.
		assert.Empty(t, mi.AllMetadata(),
			"an empty MultiIndexer stays empty; the sweep is not an indexing path")
		assert.Empty(t, logs.FilterMessage("janitor: pruned vanished worktrees").All(),
			"nothing was registered, so nothing can have been pruned")
	})

	t.Run("a nil MultiIndexer disables the janitor outright", func(t *testing.T) {
		logger, logs := janitorLogger()
		sweepNow, stop := startReconcileJanitor(nil, time.Hour, logger)
		defer stop()

		require.NotPanics(t, sweepNow)
		time.Sleep(200 * time.Millisecond)

		assert.Empty(t, sweepTriggers(logs),
			"there is no indexer to sweep, so no sweep may claim to have run")
		assert.Len(t, logs.FilterMessage("daemon: reconcile janitor disabled").All(), 1)
	})

	t.Run("a sweep after registration reaches the registered repo", func(t *testing.T) {
		g, mi, repoPath := janitorTestRepo(t, 0)
		require.NotEmpty(t, mi.AllMetadata(), "warmup has registered the repo by now")

		writeJanitorFile(t, filepath.Join(repoPath, "late.go"),
			"package main\n\nfunc LateArrival() {}\n")

		logger, _ := janitorLogger()
		sweepNow, stop := startReconcileJanitor(mi, time.Hour, logger)
		defer stop()

		sweepNow()
		require.Eventually(t, func() bool {
			return len(g.GetFileNodes("repo/late.go")) > 0
		}, 30*time.Second, 20*time.Millisecond,
			"once the indexers are registered the same kick does real work")
	})
}

// TestASaveMadeWhileTheDaemonWasDownIsQueryableAfterTheFirstSweep is the
// end-to-end statement of the defect. file_mtimes moving is the diagnostic,
// not the contract: what the blind window costs is the symbol being absent
// from the graph, so that is what this asserts — the function added while
// the daemon was down answers a name lookup after the first sweep.
func TestASaveMadeWhileTheDaemonWasDownIsQueryableAfterTheFirstSweep(t *testing.T) {
	g, mi, repoPath := janitorTestRepo(t, 0)

	require.NotEmpty(t, g.FindNodesByName("Base"),
		"the pre-shutdown content is indexed")
	require.Empty(t, g.FindNodesByName("AddedWhileTheDaemonWasDown"),
		"the offline edit is not in the graph yet")

	// Edit an already-indexed file, the way an editor save does. The mtime is
	// pushed forward explicitly: the sweep compares against the mtime recorded
	// at index time, and on a fast machine a rewrite within the same
	// filesystem timestamp tick would otherwise look unchanged.
	edited := filepath.Join(repoPath, "base.go")
	writeJanitorFile(t, edited,
		"package main\n\nfunc Base() {}\n\nfunc AddedWhileTheDaemonWasDown() { Base() }\n")
	future := time.Now().Add(2 * time.Second)
	require.NoError(t, os.Chtimes(edited, future, future))

	logger, logs := janitorLogger()
	sweepNow, stop := startReconcileJanitor(mi, time.Hour, logger)
	defer stop()

	sweepNow()

	require.Eventually(t, func() bool {
		return len(g.FindNodesByName("AddedWhileTheDaemonWasDown")) > 0
	}, 30*time.Second, 20*time.Millisecond,
		"the offline save must be queryable as a symbol after the first sweep")

	assert.NotEmpty(t, g.FindNodesByName("Base"),
		"reindexing the edited file must not drop the symbols that survived it")
	assert.Equal(t, []string{"kick"}, sweepTriggers(logs),
		"only the warmup kick can have run on a 1 h interval")
}

// TestStoppingTheJanitorWaitsForTheSweepInFlight is the shutdown-race
// regression. The daemon tears down LIFO: stopJanitor first (daemon.go:488),
// then runTeardown's state.shared.Close() (daemon.go:288). stop() used to be a
// signal rather than a join -- it closed a channel and returned -- so a sweep
// already inside ReconcileAll kept calling into the store while the store was
// being closed.
//
// That hole predates the warmup kick: the plain ticker reached it the same way.
// The kick made it far more reachable, because it fires deterministically at the
// end of warmup, and a daemon stopped shortly after warmup is the normal case in
// the restart-heavy workflow the kick exists to serve.
//
// The assertion is the contract teardown actually needs: when stop() returns, no
// sweep is running. runtimeactivity is the right witness because the sweep body
// brackets itself with Begin/End("reconcile"), so a non-zero count IS an
// in-flight sweep.
func TestStoppingTheJanitorWaitsForTheSweepInFlight(t *testing.T) {
	// Pad the repo so the sweep is long enough that stop() genuinely lands
	// mid-sweep. If it finishes first the test still passes -- it just stops
	// proving anything, so it can never fail spuriously.
	_, mi, _ := janitorTestRepo(t, 200)

	logger, logs := janitorLogger()
	sweepNow, stop := startReconcileJanitor(mi, time.Hour, logger)

	sweepNow()
	require.Eventually(t, func() bool {
		return runtimeactivity.Current().ByKind["reconcile"] > 0
	}, 30*time.Second, time.Millisecond, "a sweep must be in flight before stop is meaningful")

	stop()

	assert.Zero(t, runtimeactivity.Current().ByKind["reconcile"],
		"stop() must not return while a sweep is still calling into the store")

	// The other half, and this one is timing-free: the goroutine is provably
	// gone, so a kick that loses the shutdown race cannot start one more sweep.
	// That is the select-randomness case -- with both a queued kick and a closed
	// stop channel ready, Go picks at random -- which the pre-select context
	// check now closes.
	before := len(sweepTriggers(logs))
	sweepNow()
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, before, len(sweepTriggers(logs)),
		"a kick after stop must never start a sweep; teardown is already closing the store")
}
