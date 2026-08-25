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
