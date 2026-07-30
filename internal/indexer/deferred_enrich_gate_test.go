package indexer

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
	"github.com/zzet/gortex/internal/semantic"
)

// spyEnrichProvider is a minimal semantic.Provider that records the repo
// prefixes runDeferredEnrich dispatched it against, and optionally reports a
// Partial result. It lets the gate tests observe enrichment dispatch without
// spawning a real language server.
type spyEnrichProvider struct {
	mu      sync.Mutex
	repos   []string
	partial bool
}

func (s *spyEnrichProvider) Name() string        { return "spy" }
func (s *spyEnrichProvider) Languages() []string { return []string{"go"} }
func (s *spyEnrichProvider) Available() bool     { return true }

func (s *spyEnrichProvider) EnrichRepo(_ graph.Store, repoPrefix, _ string) (*semantic.EnrichResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repos = append(s.repos, repoPrefix)
	return &semantic.EnrichResult{Provider: "spy", Language: "go", Partial: s.partial}, nil
}

func (s *spyEnrichProvider) Enrich(g graph.Store, repoRoot string) (*semantic.EnrichResult, error) {
	return s.EnrichRepo(g, "", repoRoot)
}

func (s *spyEnrichProvider) EnrichFile(_ graph.Store, _, _ string) (*semantic.EnrichResult, error) {
	return nil, nil
}

func (s *spyEnrichProvider) Close() error { return nil }

func (s *spyEnrichProvider) invoked() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.repos...)
}

func newSpyManager(spy *spyEnrichProvider) *semantic.Manager {
	mgr := semantic.NewManager(semantic.Config{Enabled: true}, zap.NewNop())
	mgr.RegisterProvider(spy)
	return mgr
}

// newEmptyMultiIndexer builds a MultiIndexer over an empty config so a test
// can drive its unexported per-repo dispatch helpers directly.
func newEmptyMultiIndexer(t *testing.T, g graph.Store) *MultiIndexer {
	t.Helper()
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)
	return NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
}

// TestMultiIndexer_RunDeferredEnrich_GatesUnchangedRepos is the core
// warm-restart fix: the daemon collects every indexer and enriches them all,
// but only repos that actually did work (pendingEnrich set) should re-run the
// expensive semantic pass. An unchanged repo must be skipped with a log line
// rather than re-confirming its already-persisted enrichment for minutes.
func TestMultiIndexer_RunDeferredEnrich_GatesUnchangedRepos(t *testing.T) {
	g := graph.New()
	cfg := config.Default().Index

	spy := &spyEnrichProvider{}
	mgr := newSpyManager(spy)

	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)

	changed := New(g, newTestRegistry(), cfg, logger)
	changed.SetRepoPrefix("repo-changed")
	changed.SetSemanticManager(mgr)
	changed.pendingEnrich.Store(true)

	unchanged := New(g, newTestRegistry(), cfg, logger)
	unchanged.SetRepoPrefix("repo-unchanged")
	unchanged.SetSemanticManager(mgr)
	// unchanged.pendingEnrich defaults to false.

	// Drive the same per-repo dispatch the warmup's parallel enrich loop uses.
	mi := newEmptyMultiIndexer(t, g)
	mi.runDeferredEnrichParallel([]*Indexer{changed, unchanged})

	assert.Equal(t, []string{"repo-changed"}, spy.invoked(),
		"only the changed repo should have its enrichment dispatched")

	skips := logs.FilterMessage("deferred enrichment skipped").All()
	require.Len(t, skips, 1, "the unchanged repo must log exactly one skip")
	assert.Equal(t, "repo-unchanged", skips[0].ContextMap()["repo"])
	assert.Equal(t, "unchanged", skips[0].ContextMap()["reason"])

	assert.False(t, changed.pendingEnrich.Load(), "a clean enrich clears the marker")
	assert.False(t, unchanged.pendingEnrich.Load(), "the skipped repo never had a marker")
}

// TestIndexer_PendingEnrich_SetByIndexAndIncremental covers the marker
// transitions on the real index paths: IndexCtx that observed files sets it, a
// no-op incremental reconcile leaves it untouched, and an incremental pass
// that re-indexes a changed file sets it again.
func TestIndexer_PendingEnrich_SetByIndexAndIncremental(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "main.go"),
		"package main\n\nfunc main() { helper() }\n\nfunc helper() {}\n")

	g := graph.New()
	idx := newTestIndexer(g)

	_, err := idx.Index(dir)
	require.NoError(t, err)
	assert.True(t, idx.pendingEnrich.Load(),
		"IndexCtx that observed files must set pendingEnrich")

	// A zero-change reconcile must not raise the marker.
	idx.pendingEnrich.Store(false)
	_, err = idx.IncrementalReindexPaths(dir, nil)
	require.NoError(t, err)
	assert.False(t, idx.pendingEnrich.Load(),
		"a no-op IncrementalReindexPaths call must leave pendingEnrich clear")

	// A scoped incremental pass that re-indexes a stale file sets it.
	bumpMtime(t, filepath.Join(dir, "main.go"),
		"package main\n\nfunc main() { helper(); helper() }\n\nfunc helper() {}\n")
	res, err := idx.IncrementalReindexPaths(dir, []string{filepath.Join(dir, "main.go")})
	require.NoError(t, err)
	require.Positive(t, res.StaleFileCount)
	assert.True(t, idx.pendingEnrich.Load(),
		"IncrementalReindexPaths with stale files must set pendingEnrich")
}

// TestIndexer_RunDeferredEnrich_ClearsOnFullEnrich verifies the clear side of
// the contract: a fully non-partial enrich drops the marker.
func TestIndexer_RunDeferredEnrich_ClearsOnFullEnrich(t *testing.T) {
	g := graph.New()
	spy := &spyEnrichProvider{partial: false}
	idx := New(g, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetSemanticManager(newSpyManager(spy))
	idx.pendingEnrich.Store(true)

	idx.runDeferredEnrich()

	assert.Equal(t, []string{"r"}, spy.invoked(), "a pending repo must run enrichment")
	assert.False(t, idx.pendingEnrich.Load(), "a fully non-partial enrich clears the marker")
}

// TestIndexer_RunDeferredEnrich_PartialLeavesPending verifies a partial pass
// leaves the marker set so a later pass retries.
func TestIndexer_RunDeferredEnrich_PartialLeavesPending(t *testing.T) {
	g := graph.New()
	spy := &spyEnrichProvider{partial: true}
	idx := New(g, newTestRegistry(), config.Default().Index, zap.NewNop())
	idx.SetRepoPrefix("r")
	idx.SetSemanticManager(newSpyManager(spy))
	idx.pendingEnrich.Store(true)

	idx.runDeferredEnrich()

	assert.Equal(t, []string{"r"}, spy.invoked())
	assert.True(t, idx.pendingEnrich.Load(),
		"a partial enrich must leave pendingEnrich set for a later retry")
}

// TestIndexer_RunDeferredEnrich_ForceEnvBypassesGate verifies
// GORTEX_WARMUP_FORCE_ENRICH=1 runs enrichment even with no pending marker,
// and logs that it forced rather than skipping.
func TestIndexer_RunDeferredEnrich_ForceEnvBypassesGate(t *testing.T) {
	t.Setenv("GORTEX_WARMUP_FORCE_ENRICH", "1")

	g := graph.New()
	spy := &spyEnrichProvider{}
	core, logs := observer.New(zapcore.InfoLevel)
	idx := New(g, newTestRegistry(), config.Default().Index, zap.New(core))
	idx.SetRepoPrefix("r")
	idx.SetSemanticManager(newSpyManager(spy))
	// idx.pendingEnrich defaults to false — the gate would normally skip.

	idx.runDeferredEnrich()

	assert.Equal(t, []string{"r"}, spy.invoked(),
		"force env must run enrichment despite no pending marker")
	require.Len(t, logs.FilterMessage("deferred enrichment forced despite no pending changes").All(), 1,
		"the forced bypass must be logged")
	assert.Empty(t, logs.FilterMessage("deferred enrichment skipped").All(),
		"a forced run must not log a skip")
}
