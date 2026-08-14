package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/reach"
	"github.com/zzet/gortex/internal/search"
)

const batchTransitionTestTimeout = 5 * time.Second

func newBatchTransitionTestMulti(g graph.Store) *MultiIndexer {
	if g == nil {
		g = graph.New()
	}
	return NewMultiIndexer(g, parser.NewRegistry(), search.NewNull(), nil, zap.NewNop())
}

func requireBatchTransitionBlocked(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
		t.Fatal("batch transition completed while a repository mutation was active")
	case <-time.After(75 * time.Millisecond):
	}
}

func TestIndexAllWorkersDoNotReenterRegistryLock(t *testing.T) {
	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cm := newTestConfigManager(t)

	for _, name := range []string{"repo-a", "repo-b"} {
		root := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.MkdirAll(root, 0o755))
		writeFile(t, filepath.Join(root, "main.go"), "package main\nfunc "+name[len(name)-1:]+"() {}\n")
		cm.Global().Repos = append(cm.Global().Repos, config.RepoEntry{Path: root, Name: name})
	}

	mi := NewMultiIndexer(g, reg, search.NewNull(), cm, zap.NewNop())
	done := make(chan error, 1)
	go func() {
		_, err := mi.IndexAll()
		done <- err
	}()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("IndexAll deadlocked while workers constructed per-repository indexers")
	}
}

func TestParallelBatchPropagatesAndResetClearsBothIndexerFlags(t *testing.T) {
	mi := newBatchTransitionTestMulti(nil)
	existing := &Indexer{}
	mi.indexers["existing"] = existing

	mi.BeginParallelBatch()
	assert.True(t, existing.deferGlobalPasses.Load())
	assert.True(t, existing.deferResolve.Load())

	created := mi.newPerRepoIndexer(config.IndexConfig{})
	assert.True(t, created.deferGlobalPasses.Load(), "a batch-created indexer must inherit global deferral")
	assert.True(t, created.deferResolve.Load(), "a batch-created indexer must inherit resolver deferral")
	mi.mu.Lock()
	mi.indexers["created"] = created
	mi.mu.Unlock()

	mi.ResetBatch()
	for name, idx := range map[string]*Indexer{"existing": existing, "created": created} {
		assert.False(t, idx.deferGlobalPasses.Load(), "%s retained global deferral", name)
		assert.False(t, idx.deferResolve.Load(), "%s retained resolver deferral", name)
	}
}

func TestBatchTransitionWaitsForReconcileExecutor(t *testing.T) {
	mi := newBatchTransitionTestMulti(nil)
	idx := &Indexer{}
	mi.indexers["repo"] = idx
	coordinator := mi.repositoryMutationCoordinator("repo")

	started := make(chan struct{})
	release := make(chan struct{})
	coordinator.mu.Lock()
	coordinator.executor = func([]string) (*IndexResult, error) {
		close(started)
		<-release
		return &IndexResult{}, nil
	}
	coordinator.mu.Unlock()

	reconcileDone := make(chan error, 1)
	go func() {
		_, err := coordinator.reconcile(context.Background(), []string{"a.go"})
		reconcileDone <- err
	}()
	select {
	case <-started:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("reconcile executor did not start")
	}

	transitionDone := make(chan struct{})
	go func() {
		mi.BeginParallelBatch()
		close(transitionDone)
	}()
	requireBatchTransitionBlocked(t, transitionDone)

	close(release)
	require.NoError(t, <-reconcileDone)
	select {
	case <-transitionDone:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("batch transition did not resume after reconcile executor completed")
	}
	assert.True(t, idx.deferGlobalPasses.Load())
	assert.True(t, idx.deferResolve.Load())
	mi.ResetBatch()
}

func TestBatchTransitionWaitsForExclusiveCallback(t *testing.T) {
	mi := newBatchTransitionTestMulti(nil)
	idx := &Indexer{}
	mi.indexers["repo"] = idx
	coordinator := mi.repositoryMutationCoordinator("repo")

	started := make(chan struct{})
	release := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- coordinator.runExclusive(context.Background(), func() error {
			close(started)
			<-release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("exclusive mutation did not start")
	}

	transitionDone := make(chan struct{})
	go func() {
		mi.BeginBatch()
		close(transitionDone)
	}()
	requireBatchTransitionBlocked(t, transitionDone)

	close(release)
	require.NoError(t, <-mutationDone)
	select {
	case <-transitionDone:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("batch transition did not resume after exclusive mutation completed")
	}
	assert.True(t, idx.deferGlobalPasses.Load())
	assert.False(t, idx.deferResolve.Load())
	mi.ResetBatch()
}

func TestQueuedMutationReappliesModeAfterTransition(t *testing.T) {
	mi := newBatchTransitionTestMulti(nil)
	idx := &Indexer{}
	coordinator := mi.repositoryMutationCoordinator("repo")

	mi.batchMutationGate.Lock()
	submitted := make(chan struct{})
	observed := make(chan multiIndexerBatchMode, 1)
	mutationDone := make(chan error, 1)
	go func() {
		close(submitted)
		mutationDone <- coordinator.runExclusive(context.Background(), func() error {
			observed <- mi.reapplyBatchModeForMutation(idx)
			return nil
		})
	}()
	<-submitted
	select {
	case <-observed:
		mi.batchMutationGate.Unlock()
		t.Fatal("queued mutation crossed the transition write gate")
	case <-time.After(75 * time.Millisecond):
	}

	// Model the flag update performed by BeginParallelBatch without propagating
	// to this not-yet-registered Indexer. Its guarded callback must close that
	// construction-to-admission window from the authoritative MultiIndexer mode.
	mi.mu.Lock()
	mi.deferGlobalPasses = true
	mi.deferResolve = true
	mi.mu.Unlock()
	mi.batchMutationGate.Unlock()

	var mode multiIndexerBatchMode
	select {
	case mode = <-observed:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("queued mutation did not resume after transition")
	}
	require.NoError(t, <-mutationDone)
	assert.True(t, mode.deferGlobalPasses)
	assert.True(t, mode.deferResolve)
	assert.True(t, idx.deferGlobalPasses.Load())
	assert.True(t, idx.deferResolve.Load())
	mi.ResetBatch()
}

type blockingBatchCheckpointStore struct {
	graph.Store
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (s *blockingBatchCheckpointStore) CheckpointWAL() error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

func TestEndBatchHoldsTransitionGateThroughGlobalPass(t *testing.T) {
	releaseCheckpoint := make(chan struct{})
	store := &blockingBatchCheckpointStore{
		Store:   graph.New(),
		entered: make(chan struct{}),
		release: releaseCheckpoint,
	}
	mi := newBatchTransitionTestMulti(store)
	store.AddNode(&graph.Node{ID: "sink", Kind: graph.KindFunction, Name: "sink", FilePath: "sink.go"})
	_, _, _, hit := reach.Lookup(store, "sink")
	require.True(t, hit)
	beforeBuild := reach.BuildCounter()
	mi.BeginBatch()

	endDone := make(chan struct{})
	go func() {
		mi.EndBatch()
		close(endDone)
	}()
	select {
	case <-store.entered:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("EndBatch did not reach the global-pass checkpoint")
	}

	coordinator := mi.repositoryMutationCoordinator("repo")
	callbackRan := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- coordinator.runExclusive(context.Background(), func() error {
			close(callbackRan)
			return nil
		})
	}()
	select {
	case <-callbackRan:
		t.Fatal("repository mutation ran while EndBatch global pass held the write gate")
	case <-time.After(75 * time.Millisecond):
	}

	lookupStarted := make(chan struct{})
	lookupDone := make(chan struct{})
	go func() {
		close(lookupStarted)
		reach.Lookup(store, "sink")
		close(lookupDone)
	}()
	<-lookupStarted
	select {
	case <-lookupDone:
		t.Fatal("reach lookup escaped the EndBatch topology transaction")
	case <-time.After(75 * time.Millisecond):
	}

	close(releaseCheckpoint)
	select {
	case <-endDone:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("EndBatch did not finish after checkpoint release")
	}
	require.NoError(t, <-mutationDone)
	select {
	case <-callbackRan:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("repository mutation did not resume after EndBatch")
	}
	select {
	case <-lookupDone:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("reach lookup did not resume after EndBatch publication")
	}
	require.Greater(t, reach.BuildCounter(), beforeBuild)
}

type blockingBatchPurgeStore struct {
	graph.Store
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (s *blockingBatchPurgeStore) PurgeRepo(string) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

func TestUntrackTeardownBlocksReachLookup(t *testing.T) {
	releasePurge := make(chan struct{})
	store := &blockingBatchPurgeStore{
		Store:   graph.New(),
		entered: make(chan struct{}),
		release: releasePurge,
	}
	mi := newBatchTransitionTestMulti(store)
	store.AddNode(&graph.Node{ID: "sink", Kind: graph.KindFunction, Name: "sink", FilePath: "sink.go"})
	_, _, _, hit := reach.Lookup(store, "sink")
	require.True(t, hit)
	beforeBuild := reach.BuildCounter()
	coordinator := mi.repositoryMutationCoordinator("repo")
	idx := &Indexer{}
	idx.attachRepositoryMutationCoordinator(coordinator)
	mi.mu.Lock()
	mi.indexers["repo"] = idx
	mi.repos["repo"] = &RepoMetadata{RepoPrefix: "repo", NodeCount: 1, EdgeCount: 2}
	mi.mu.Unlock()

	type removalCounts struct{ nodes, edges int }
	untrackDone := make(chan removalCounts, 1)
	go func() {
		nodes, edges := mi.UntrackRepo("repo")
		untrackDone <- removalCounts{nodes: nodes, edges: edges}
	}()
	select {
	case <-store.entered:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("UntrackRepo did not reach the repository purge")
	}

	lookupStarted := make(chan struct{})
	lookupDone := make(chan struct{})
	go func() {
		close(lookupStarted)
		reach.Lookup(store, "sink")
		close(lookupDone)
	}()
	<-lookupStarted
	select {
	case <-lookupDone:
		t.Fatal("reach lookup escaped an in-flight untrack topology transaction")
	case <-time.After(75 * time.Millisecond):
	}

	close(releasePurge)
	select {
	case removed := <-untrackDone:
		assert.Equal(t, removalCounts{nodes: 1, edges: 2}, removed)
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("UntrackRepo did not finish after purge release")
	}
	select {
	case <-lookupDone:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("reach lookup did not resume after untrack publication")
	}
	require.Greater(t, reach.BuildCounter(), beforeBuild)
}
