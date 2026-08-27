package indexer

import (
	"context"
	"os"
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
	"github.com/zzet/gortex/internal/search"
)

const rederiveStartLog = "workspace derivation starting (post-track)"

// newRederiveTestIndexer builds a MultiIndexer over an empty workspace
// with an observed logger, so a test can count derivation passes by their
// breadcrumb instead of reaching into scheduler internals.
func newRederiveTestIndexer(t *testing.T, debounce time.Duration) (*MultiIndexer, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zapcore.InfoLevel)

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewNull(), cm, zap.New(core))
	mi.rederive.debounce = debounce
	return mi, logs
}

// setupDependentRepo creates a repo whose module requires dep and calls
// into it, so the workspace holds a genuine cross-repo reference.
func setupDependentRepo(t *testing.T, name, depModule, depPath string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeFile(t, filepath.Join(dir, "go.mod"),
		"module example.com/"+name+"\n\ngo 1.22\n\nrequire example.com/"+depModule+" v0.0.0\n\nreplace example.com/"+depModule+" => "+depPath+"\n")
	writeFile(t, filepath.Join(dir, "app.go"), `package main

import lib "example.com/`+depModule+`"

func Run() string { return lib.RunGreet(lib.EnglishGreeter{}) }
`)
	return dir
}

// crossRepoEdgeKinds counts edges whose endpoints sit in different repos.
func crossRepoEdgeKinds(g graph.Store) map[graph.EdgeKind]int {
	out := map[graph.EdgeKind]int{}
	for _, e := range g.AllEdges() {
		from := strings.SplitN(e.From, "/", 2)[0]
		to := strings.SplitN(e.To, "/", 2)[0]
		if from != "" && to != "" && from != to {
			out[e.Kind]++
		}
	}
	return out
}

// The regression this exists for: a repository tracked outside a batch
// used to join the graph carrying only the edges its own extraction
// produced. The workspace-wide passes ran only at EndBatch, so nothing
// derived EdgeTests / EdgeImplements for it — and no later daemon restart
// recovered them, because a warm restart sees nodes already on disk and
// takes the fast path that skips the passes.
func TestTrackRepoCtx_RunsWorkspaceDerivationOutsideBatch(t *testing.T) {
	repo := setupRepoWithTestAndIface(t, "repo-a")
	mi, logs := newRederiveTestIndexer(t, 0)

	result, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repo, Name: "repo-a"})
	require.NoError(t, err)
	require.NotNil(t, result)

	mi.WaitWorkspaceRederive()

	assert.Equal(t, 1, logs.FilterMessage(rederiveStartLog).Len(),
		"a track outside a batch must run the workspace derivation exactly once")
	// Both edge kinds are produced only by the global passes — the
	// per-repo indexer never emits them.
	assert.Greater(t, countEdges(mi.graph, graph.EdgeTests), 0,
		"EdgeTests is derived by the global test-edge pass; without it the repo lands unbound")
	assert.Greater(t, countEdges(mi.graph, graph.EdgeImplements), 0,
		"EdgeImplements is derived by the global inference pass")
}

// The symptom, not just the mechanism. A cross-repo edge is derived by a
// workspace-wide pass, never by either repo's own extraction, so tracking
// the second repo of a dependent pair outside a batch used to leave the
// two repos side by side in one graph with nothing joining them — the
// shape an untrack + track of a shared dependency leaves behind.
func TestTrackRepoCtx_BindsCrossRepoEdgesForRepoTrackedLater(t *testing.T) {
	lib := setupRepoWithTestAndIface(t, "repo-a")
	app := setupDependentRepo(t, "repo-b", "repo-a", lib)
	mi, _ := newRederiveTestIndexer(t, 0)

	for _, r := range []config.RepoEntry{{Path: lib, Name: "repo-a"}, {Path: app, Name: "repo-b"}} {
		_, err := mi.TrackRepoCtx(context.Background(), r)
		require.NoError(t, err)
		mi.WaitWorkspaceRederive()
	}

	assert.Positive(t, crossRepoEdgeKinds(mi.graph)[graph.EdgeCrossRepoCalls],
		"repo-b's call into repo-a must carry its cross-repo edge; without the post-track derivation the pair stays unjoined")
}

// A repo tracked inside a batch must NOT schedule its own pass: EndBatch
// runs one for the whole batch, and paying a full workspace pass per repo
// would turn an N-repo warmup into N full passes.
func TestTrackRepoCtx_BatchedTrackDefersToEndBatch(t *testing.T) {
	repo := setupRepoWithTestAndIface(t, "repo-a")
	mi, logs := newRederiveTestIndexer(t, 0)

	mi.BeginBatch()
	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repo, Name: "repo-a"})
	require.NoError(t, err)
	mi.WaitWorkspaceRederive()
	assert.Zero(t, logs.FilterMessage(rederiveStartLog).Len(),
		"a batched track must leave the passes to EndBatch")

	mi.EndBatch()
	assert.Greater(t, countEdges(mi.graph, graph.EdgeTests), 0,
		"EndBatch still derives the batch's edges")
}

// A burst of tracks — `daemon reload` adopting several repos — must
// collapse. The pass is minutes long on a real workspace, so the cost of
// running one per repo is what makes the run slot plus the single queued
// slot load-bearing rather than an optimisation.
func TestScheduleWorkspaceRederive_CoalescesBurst(t *testing.T) {
	mi, logs := newRederiveTestIndexer(t, 40*time.Millisecond)

	for i := 0; i < 10; i++ {
		mi.scheduleWorkspaceRederive("repo-a")
	}
	mi.WaitWorkspaceRederive()

	runs := logs.FilterMessage(rederiveStartLog).Len()
	assert.GreaterOrEqual(t, runs, 1, "the burst must produce at least one pass")
	assert.LessOrEqual(t, runs, 2,
		"ten tracks must collapse to the run slot plus at most one queued pass, not ten")
	assert.False(t, mi.WorkspaceRederivePending(), "the scheduler must return to idle")
}

// A track landing while a pass is in flight has to earn another one: the
// running pass may already have read past that repository's nodes.
func TestScheduleWorkspaceRederive_RequeuesWorkArrivingMidPass(t *testing.T) {
	mi, logs := newRederiveTestIndexer(t, 0)

	mi.rederive.mu.Lock()
	mi.rederive.running = true // pretend a pass is in flight
	mi.rederive.mu.Unlock()

	mi.scheduleWorkspaceRederive("repo-b")
	assert.Zero(t, logs.FilterMessage(rederiveStartLog).Len(),
		"no second goroutine may start while one is running")

	mi.rederive.mu.Lock()
	queued := mi.rederive.queued
	mi.rederive.mu.Unlock()
	assert.True(t, queued, "the arriving track must be queued for another pass")
}

const (
	rederiveDoneLog        = "workspace derivation complete (post-track)"
	globalPassesDoneLog    = "global passes complete"
	globalPassesAbortedLog = "global passes preempted"
)

// installFakeInFlightRederive puts the scheduler into the state a real
// pass holds: running, cancellable, and owning the batch-transition write
// side. The goroutine releases the gate only when its context is
// cancelled, which is exactly the stall a topology mutation used to sit
// behind. Returns a func the test can use to observe that it unwound.
func installFakeInFlightRederive(t *testing.T, mi *MultiIndexer) (released <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	s := &mi.rederive
	s.mu.Lock()
	s.running = true
	s.cancel = cancel
	s.wg.Add(1)
	s.mu.Unlock()

	started := make(chan struct{})
	go func() {
		defer s.wg.Done()
		defer close(done)
		mi.batchMutationGate.Lock()
		close(started)
		<-ctx.Done()
		mi.batchMutationGate.Unlock()
		s.mu.Lock()
		s.running = false
		s.cancel = nil
		s.mu.Unlock()
	}()
	<-started
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return done
}

// The regression. UntrackRepo takes the batch-transition read side and the
// reachability topology writer, both of which a background derivation
// holds for its whole run. Before preemption an untrack issued during one
// waited the pass out — 19.7 minutes on a two-repo workspace, and
// `daemon stop` had to force-kill. It must now stand the pass down first.
func TestUntrackRepo_PreemptsWorkspaceDerivation(t *testing.T) {
	repo := setupRepoWithTestAndIface(t, "repo-a")
	mi, _ := newRederiveTestIndexer(t, 0)

	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repo, Name: "repo-a"})
	require.NoError(t, err)
	mi.WaitWorkspaceRederive()

	released := installFakeInFlightRederive(t, mi)

	untracked := make(chan struct{})
	go func() {
		mi.UntrackRepo("repo-a")
		close(untracked)
	}()

	select {
	case <-untracked:
	case <-time.After(10 * time.Second):
		t.Fatal("UntrackRepo blocked behind the in-flight workspace derivation")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("the derivation was never cancelled")
	}

	mi.rederive.mu.Lock()
	queued := mi.rederive.queued
	mi.rederive.mu.Unlock()
	assert.True(t, queued,
		"the abandoned derivation is still owed to the graph and must be re-queued")
}

// A batch transition takes the same write side, so it needs the same
// courtesy — otherwise a reload issued mid-derivation stalls the daemon.
func TestBeginBatch_PreemptsWorkspaceDerivation(t *testing.T) {
	mi, _ := newRederiveTestIndexer(t, 0)
	released := installFakeInFlightRederive(t, mi)

	begun := make(chan struct{})
	go func() {
		mi.BeginBatch()
		close(begun)
	}()

	select {
	case <-begun:
	case <-time.After(10 * time.Second):
		t.Fatal("BeginBatch blocked behind the in-flight workspace derivation")
	}
	<-released
}

// Close must cancel the pass, not wait for it. Waiting is what made
// `daemon stop` take 153s and then force-kill.
func TestStopWorkspaceRederive_CancelsInsteadOfWaiting(t *testing.T) {
	mi, _ := newRederiveTestIndexer(t, 0)
	installFakeInFlightRederive(t, mi)

	stopped := make(chan struct{})
	go func() {
		mi.stopWorkspaceRederive()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("stopWorkspaceRederive waited for the pass instead of cancelling it")
	}

	assert.False(t, mi.WorkspaceRederivePending())
	mi.scheduleWorkspaceRederive("repo-a")
	mi.WaitWorkspaceRederive()
	assert.False(t, mi.WorkspaceRederivePending(),
		"a closed scheduler must refuse new passes")
}

// The boundary that makes preemption work at all: the global passes have
// to observe the cancelled context, or cancelling only sets a flag nobody
// reads and the gates stay held to the end.
func TestRunGlobalGraphPasses_ReturnsAtFirstBoundaryWhenCancelled(t *testing.T) {
	repo := setupRepoWithTestAndIface(t, "repo-a")
	mi, logs := newRederiveTestIndexer(t, 0)

	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repo, Name: "repo-a"})
	require.NoError(t, err)
	mi.WaitWorkspaceRederive()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := logs.FilterMessage(globalPassesDoneLog).Len()
	mi.runGlobalGraphPasses(ctx, nil, false, false)

	aborts := logs.FilterMessage(globalPassesAbortedLog).All()
	require.Len(t, aborts, 1, "a cancelled run must log exactly one preemption")
	assert.Equal(t, "infer_implements", aborts[0].ContextMap()["before_pass"],
		"the very first boundary must be the one that returns")
	assert.Equal(t, before, logs.FilterMessage(globalPassesDoneLog).Len(),
		"a preempted run must not claim it completed")
}

// An uncancelled run must be untouched by any of the above — EndBatch and
// the cold-index path both pass context.Background().
func TestRunGlobalGraphPasses_UncancelledRunStillCompletes(t *testing.T) {
	repo := setupRepoWithTestAndIface(t, "repo-a")
	mi, logs := newRederiveTestIndexer(t, 0)

	_, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repo, Name: "repo-a"})
	require.NoError(t, err)
	mi.WaitWorkspaceRederive()

	before := logs.FilterMessage(globalPassesDoneLog).Len()
	mi.runGlobalGraphPasses(context.Background(), nil, false, false)
	assert.Equal(t, before+1, logs.FilterMessage(globalPassesDoneLog).Len())
	assert.Zero(t, logs.FilterMessage(globalPassesAbortedLog).Len())
}

// runWorkspaceRederive must report the difference, so a status reader is
// never told a preempted pass bound the graph.
func TestRunWorkspaceRederive_ReportsPreemption(t *testing.T) {
	mi, logs := newRederiveTestIndexer(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	mi.runWorkspaceRederive(ctx, map[string]struct{}{"repo-a": {}})
	assert.Zero(t, logs.FilterMessage(rederiveStartLog).Len(),
		"an already-cancelled pass must not even start")

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	mi.runWorkspaceRederive(ctx2, map[string]struct{}{"repo-a": {}})
	done := logs.FilterMessage(rederiveDoneLog).All()
	require.Len(t, done, 1)
	assert.Equal(t, false, done[0].ContextMap()["preempted"])
}
