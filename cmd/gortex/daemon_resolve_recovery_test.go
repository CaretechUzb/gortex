package main

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/parser"
)

type warmupResolveStateStore struct {
	graph.Store
	token graph.ResolveStateToken
	err   error
}

func (store *warmupResolveStateStore) BeginResolvePass() (graph.ResolveStateToken, error) {
	return store.token, store.err
}

func (store *warmupResolveStateStore) ResolvePassIncomplete() (graph.ResolveStateToken, bool, error) {
	if store.err != nil {
		return graph.ResolveStateToken{}, false, store.err
	}
	return store.token, store.token.Generation != 0, nil
}

func (store *warmupResolveStateStore) CompleteResolvePass(graph.ResolveStateToken) error {
	return store.err
}

func TestWarmupResolveRecoveryPolicyForcesFullReconstruction(t *testing.T) {
	recovery := warmupResolveRecovery{
		token:    graph.ResolveStateToken{Generation: 73},
		required: true,
	}
	prior := map[string]int64{"repo/a.go": 1}
	scope := map[string]struct{}{"repo": {}}

	if got := recovery.priorMtimes(prior); got != nil {
		t.Fatalf("prior mtimes = %v, want nil full-track sentinel", got)
	}
	if !recovery.anyChanged(0) {
		t.Fatal("recovery did not force the global resolve path")
	}
	if recovery.exactDelta(true) {
		t.Fatal("recovery retained the exact warm-delta path")
	}
	if got := recovery.resolveScope(scope); got != nil {
		t.Fatalf("resolve scope = %v, want nil whole-graph scope", got)
	}
}

func TestProbeWarmupResolveRecoveryFailsClosed(t *testing.T) {
	probeErr := errors.New("resolve_state unavailable")
	store := &warmupResolveStateStore{Store: graph.New(), err: probeErr}
	if _, err := probeWarmupResolveRecovery(store); !errors.Is(err, probeErr) {
		t.Fatalf("probe error = %v, want %v", err, probeErr)
	}
}

func TestProbeWarmupResolveRecoveryPreservesObservedGeneration(t *testing.T) {
	store := &warmupResolveStateStore{
		Store: graph.New(),
		token: graph.ResolveStateToken{Generation: 91},
	}
	recovery, err := probeWarmupResolveRecovery(store)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.required || recovery.token.Generation != 91 || recovery.token.Owned {
		t.Fatalf("recovery = %+v, want required observed generation 91", recovery)
	}
}

func TestWarmupDaemonStateProbeFailureFailsClosed(t *testing.T) {
	probeErr := errors.New("injected resolve-state probe failure")
	store := &warmupResolveStateStore{Store: graph.New(), err: probeErr}
	state := &daemonState{
		graph:         store,
		multiIndexer:  &indexer.MultiIndexer{},
		configManager: &config.ConfigManager{},
	}
	controller := &realController{}
	readyCalls := 0

	watcher, timings, err := warmupDaemonState(state, zap.NewNop(), func() {
		readyCalls++
		controller.MarkReady(0)
	})

	require.ErrorIs(t, err, probeErr)
	assert.Nil(t, watcher)
	require.NotNil(t, timings)
	assert.Zero(t, readyCalls)
	assert.False(t, controller.IsReady())
	assert.False(t, controller.IsEnriched())
	assert.Nil(t, controller.watcher())
}

func TestWarmupDaemonStateRecoveryTrackFailureFailsClosed(t *testing.T) {
	dir := t.TempDir()
	cm, err := config.NewConfigManager(filepath.Join(dir, "config.yaml"))
	require.NoError(t, err)
	require.NoError(t, cm.Global().AddRepo(config.RepoEntry{
		Path: filepath.Join(dir, "missing-repository"),
	}))

	store := &warmupResolveStateStore{
		Store: graph.New(),
		token: graph.ResolveStateToken{Generation: 117},
	}
	reg := parser.NewRegistry()
	idx := indexer.New(store, reg, config.Default().Index, zap.NewNop())
	mi := indexer.NewMultiIndexer(store, reg, idx.Search(), cm, zap.NewNop())
	state := &daemonState{
		graph:         store,
		indexer:       idx,
		multiIndexer:  mi,
		configManager: cm,
	}
	controller := &realController{}
	readyCalls := 0

	watcher, timings, err := warmupDaemonState(state, zap.NewNop(), func() {
		readyCalls++
		controller.MarkReady(0)
	})

	require.Error(t, err)
	assert.ErrorContains(t, err, "repository reconstruction failed")
	assert.Nil(t, watcher)
	require.NotNil(t, timings)
	assert.Zero(t, readyCalls)
	assert.False(t, controller.IsReady())
	assert.False(t, controller.IsEnriched())
	assert.Nil(t, controller.watcher())
	_, incomplete, probeErr := store.ResolvePassIncomplete()
	require.NoError(t, probeErr)
	assert.True(t, incomplete, "failed recovery must retain its durable generation")
}
