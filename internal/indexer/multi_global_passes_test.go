package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// setupRepoWithTestAndIface creates a repo with an interface, a type
// satisfying it, a production function, and a test function calling that
// production function. Used to assert EdgeImplements + EdgeTests get
// emitted by the global graph passes.
func setupRepoWithTestAndIface(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/"+name+"\n\ngo 1.22\n")
	writeFile(t, filepath.Join(dir, "main.go"), `package main

type Greeter interface {
	Greet() string
}

type EnglishGreeter struct{}

func (e EnglishGreeter) Greet() string { return "hello" }

func RunGreet(g Greeter) string { return g.Greet() }
`)
	writeFile(t, filepath.Join(dir, "main_test.go"), `package main

import "testing"

func TestRunGreet(t *testing.T) {
	got := RunGreet(EnglishGreeter{})
	if got != "hello" {
		t.Fatalf("want hello, got %q", got)
	}
}
`)
	return dir
}

func countEdges(g graph.Store, kind graph.EdgeKind) int {
	n := 0
	for _, e := range g.AllEdges() {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// TestMultiIndexer_IndexAll_GlobalPassesProduceEdges verifies the multi-
// repo orchestrator runs the graph-wide derivation passes (test edges,
// implements) once after the per-repo deferred loop, leaving the same
// edges in place that the old per-repo loop produced.
func TestMultiIndexer_IndexAll_GlobalPassesProduceEdges(t *testing.T) {
	repoA := setupRepoWithTestAndIface(t, "repo-a")
	repoB := setupRepoWithTestAndIface(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	results, err := mi.IndexAll()
	require.NoError(t, err)
	require.Len(t, results, 2)

	// EdgeImplements: each repo has EnglishGreeter satisfying Greeter.
	// At least one per repo (InferImplements walks the full shared
	// graph; cross-repo matches via interface name only would multiply,
	// but the names are the same in both repos so only same-repo
	// satisfiers count via the identical method-set check).
	assert.Greater(t, countEdges(g, graph.EdgeImplements), 0,
		"expected EdgeImplements after IndexAll")

	// EdgeTests: TestRunGreet calls RunGreet — one per repo.
	assert.GreaterOrEqual(t, countEdges(g, graph.EdgeTests), 2,
		"expected at least one EdgeTests per repo after IndexAll")

	// Test-marker stamped on the test function nodes.
	testFuncs := 0
	for _, n := range g.AllNodes() {
		if n.Kind != graph.KindFunction && n.Kind != graph.KindMethod {
			continue
		}
		if isTest, _ := n.Meta["is_test"].(bool); isTest {
			testFuncs++
		}
	}
	assert.GreaterOrEqual(t, testFuncs, 2, "is_test should be stamped on TestRunGreet in each repo")
}

// TestMultiIndexer_GlobalGraphPassPipeline_Idempotent verifies that running
// the live global-pass pipeline a second time does not mutate edge counts
// (graph dedup + resolver passes skip already-present edges).
func TestMultiIndexer_GlobalGraphPassPipeline_Idempotent(t *testing.T) {
	repoA := setupRepoWithTestAndIface(t, "repo-a")
	repoB := setupRepoWithTestAndIface(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{
		Repos: []config.RepoEntry{
			{Path: repoA, Name: "repo-a"},
			{Path: repoB, Name: "repo-b"},
		},
	}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)

	implsBefore := countEdges(g, graph.EdgeImplements)
	testsBefore := countEdges(g, graph.EdgeTests)
	overridesBefore := countEdges(g, graph.EdgeOverrides)
	require.Greater(t, implsBefore, 0)
	require.Greater(t, testsBefore, 0)

	// Re-run the global passes. None of the three should add duplicates.
	mi.runGlobalGraphPasses(context.Background(), nil, false, false)
	mi.runGlobalGraphPasses(context.Background(), nil, false, false)

	assert.Equal(t, implsBefore, countEdges(g, graph.EdgeImplements),
		"InferImplements re-emission should be idempotent")
	assert.Equal(t, overridesBefore, countEdges(g, graph.EdgeOverrides),
		"InferOverrides re-emission should be idempotent")
	assert.Equal(t, testsBefore, countEdges(g, graph.EdgeTests),
		"markTestSymbolsAndEmitEdges re-emission should be idempotent")
}

// TestMultiIndexer_BeginEndBatch_DefersGlobalPasses asserts that batch
// mode actually skips per-repo execution of the global passes and that
// EndBatch fills them in. Uses TrackRepoCtx (the warmup path), which is
// the loop where the O(R²) regression originated.
func TestMultiIndexer_BeginEndBatch_DefersGlobalPasses(t *testing.T) {
	repoA := setupRepoWithTestAndIface(t, "repo-a")
	repoB := setupRepoWithTestAndIface(t, "repo-b")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	mi.BeginBatch()

	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repoA, Name: "repo-a"})
	require.NoError(t, err)
	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repoB, Name: "repo-b"})
	require.NoError(t, err)

	// Mid-batch: global passes should NOT have run, so EdgeTests and
	// EdgeImplements should both be absent.
	assert.Zero(t, countEdges(g, graph.EdgeTests),
		"EdgeTests should not be emitted while BeginBatch is active")
	assert.Zero(t, countEdges(g, graph.EdgeImplements),
		"EdgeImplements should not be emitted while BeginBatch is active")

	mi.EndBatch()

	assert.Greater(t, countEdges(g, graph.EdgeTests), 0,
		"EdgeTests should be emitted after EndBatch")
	assert.Greater(t, countEdges(g, graph.EdgeImplements), 0,
		"EdgeImplements should be emitted after EndBatch")
}

// TestMultiIndexer_TrackRepoCtx_NoBatch_RunsGlobalPassesInline asserts
// the default (non-batch) TrackRepoCtx path still runs the global
// derivation passes inline — ad-hoc single-track callers must not
// regress.
func TestMultiIndexer_TrackRepoCtx_NoBatch_RunsGlobalPassesInline(t *testing.T) {
	repoA := setupRepoWithTestAndIface(t, "repo-a")

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewNull(), cm, zap.NewNop())

	_, err = mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: repoA, Name: "repo-a"})
	require.NoError(t, err)

	// Without BeginBatch, the inline path inside IndexCtx should have
	// emitted EdgeTests and EdgeImplements already.
	assert.Greater(t, countEdges(g, graph.EdgeTests), 0,
		"non-batched TrackRepoCtx must emit EdgeTests inline")
	assert.Greater(t, countEdges(g, graph.EdgeImplements), 0,
		"non-batched TrackRepoCtx must emit EdgeImplements inline")
}
