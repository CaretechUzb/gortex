package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
	"github.com/zzet/gortex/internal/reach"
	"github.com/zzet/gortex/internal/search"
)

func TestIndexCtxStaleHandleUsesCurrentIndexerAndMetadata(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Existing() {}\n")

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cm := newTestConfigManager(t)
	mi := NewMultiIndexer(g, reg, search.NewBM25(), cm, zap.NewNop())
	entry := config.RepoEntry{Path: root, Name: "repo"}

	beforeTrack := reach.BuildCounter()
	_, err := mi.TrackRepo(entry)
	require.NoError(t, err)
	require.Greater(t, reach.BuildCounter(), beforeTrack)
	stale := mi.GetIndexer("repo")
	require.NotNil(t, stale)

	beforeReplace := reach.BuildCounter()
	_, err = mi.IndexRepo("repo")
	require.NoError(t, err)
	require.Greater(t, reach.BuildCounter(), beforeReplace)
	current := mi.GetIndexer("repo")
	require.NotNil(t, current)
	require.NotSame(t, stale, current)

	newPath := filepath.Join(root, "new.go")
	writeFile(t, newPath, "package sample\nfunc Added() {}\n")
	newKey := stale.relKey(newPath)
	_, staleHadNewFile := stale.FileMtimes()[newKey]
	require.False(t, staleHadNewFile)

	beforeIndex := reach.BuildCounter()
	result, err := stale.IndexCtx(context.Background(), root)
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Greater(t, reach.BuildCounter(), beforeIndex)
	require.Same(t, current, mi.GetIndexer("repo"))
	_, staleHasNewFile := stale.FileMtimes()[newKey]
	require.False(t, staleHasNewFile, "stale receiver must remain untouched")
	_, currentHasNewFile := current.FileMtimes()[newKey]
	require.True(t, currentHasNewFile, "live replacement must receive the full-tree index")

	meta := mi.GetMetadata("repo")
	require.NotNil(t, meta)
	require.Equal(t, result.FileCount, meta.FileCount)
	require.Equal(t, result.NodeCount, meta.NodeCount)
	require.Equal(t, result.EdgeCount, meta.EdgeCount)
	_, metadataHasNewFile := mi.FileMtimes("repo")[newKey]
	require.True(t, metadataHasNewFile)
}

func TestReconcileTopologyGenerationChangesOnlyForRealMutation(t *testing.T) {
	root := t.TempDir()
	mainPath := filepath.Join(root, "main.go")
	writeFile(t, mainPath, "package sample\nfunc Existing() {}\n")

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cm := newTestConfigManager(t)
	entry := config.RepoEntry{Path: root, Name: "repo"}
	seed := NewMultiIndexer(g, reg, search.NewBM25(), cm, zap.NewNop())
	_, err := seed.TrackRepo(entry)
	require.NoError(t, err)
	prior := seed.GetIndexer("repo").FileMtimes()

	writeFile(t, filepath.Join(root, "added.go"), "package sample\nfunc Added() {}\n")
	changed := NewMultiIndexer(g, reg, search.NewBM25(), cm, zap.NewNop())
	beforeChanged := reach.BuildCounter()
	changedResult, err := changed.ReconcileRepoCtx(context.Background(), entry, prior)
	require.NoError(t, err)
	require.NotNil(t, changedResult)
	require.Greater(t, reach.BuildCounter(), beforeChanged)

	unchangedPrior := changed.GetIndexer("repo").FileMtimes()
	unchanged := NewMultiIndexer(g, reg, search.NewBM25(), cm, zap.NewNop())
	beforeUnchanged := reach.BuildCounter()
	unchangedResult, err := unchanged.ReconcileRepoCtx(context.Background(), entry, unchangedPrior)
	require.NoError(t, err)
	require.NotNil(t, unchangedResult)
	require.False(t, incrementalTopologyChanged(unchangedResult))
	require.Equal(t, beforeUnchanged, reach.BuildCounter(), "unchanged hourly reconciliation must not flush lazy reach")
}

type armableBlockingAddBatchStore struct {
	graph.Store
	mu        sync.Mutex
	remaining int
	entered   chan struct{}
	release   chan struct{}
}

func (s *armableBlockingAddBatchStore) arm(blocks int) (<-chan struct{}, chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.remaining = blocks
	s.entered = make(chan struct{}, blocks)
	s.release = make(chan struct{})
	return s.entered, s.release
}

func (s *armableBlockingAddBatchStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.mu.Lock()
	shouldBlock := s.remaining > 0
	entered := s.entered
	release := s.release
	if shouldBlock {
		s.remaining--
	}
	s.mu.Unlock()
	if shouldBlock {
		entered <- struct{}{}
		<-release
	}
	s.Store.AddBatch(nodes, edges)
}

func TestIndexAllHoldsEveryLaneAndTopologyThroughPublication(t *testing.T) {
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cm := newTestConfigManager(t)
	for _, name := range []string{"repo-a", "repo-b"} {
		root := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.MkdirAll(root, 0o755))
		writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Existing() {}\n")
		cm.Global().Repos = append(cm.Global().Repos, config.RepoEntry{Path: root, Name: name})
	}
	store := &armableBlockingAddBatchStore{Store: graph.New()}
	mi := NewMultiIndexer(store, reg, search.NewBM25(), cm, zap.NewNop())
	_, err := mi.IndexAll()
	require.NoError(t, err)
	oldA := mi.GetIndexer("repo-a")
	require.NotNil(t, oldA)
	coordinatorA := mi.repositoryMutationCoordinator("repo-a")

	store.AddNode(&graph.Node{ID: "topology-sink", Kind: graph.KindFunction, Name: "sink", FilePath: "sink.go"})
	_, _, _, hit := reach.Lookup(store, "topology-sink")
	require.True(t, hit)
	beforeBuild := reach.BuildCounter()
	entered, release := store.arm(2)

	indexDone := make(chan error, 1)
	go func() {
		_, indexErr := mi.IndexAll()
		indexDone <- indexErr
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(batchTransitionTestTimeout):
			t.Fatal("parallel IndexAll workers did not both reach AddBatch")
		}
	}

	callbackRan := make(chan *Indexer, 1)
	callbackDone := make(chan error, 1)
	go func() {
		callbackDone <- coordinatorA.runExclusive(context.Background(), func() error {
			callbackRan <- mi.GetIndexer("repo-a")
			return nil
		})
	}()
	select {
	case <-callbackRan:
		t.Fatal("same-repository mutation crossed the active IndexAll lane")
	case <-time.After(75 * time.Millisecond):
	}

	lookupStarted := make(chan struct{})
	lookupDone := make(chan struct{})
	go func() {
		close(lookupStarted)
		reach.Lookup(store, "topology-sink")
		close(lookupDone)
	}()
	<-lookupStarted
	select {
	case <-lookupDone:
		t.Fatal("reach lookup escaped the active IndexAll topology transaction")
	case <-time.After(75 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-indexDone:
		require.NoError(t, err)
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("IndexAll did not finish after AddBatch release")
	}
	require.NoError(t, <-callbackDone)
	var callbackIndexer *Indexer
	select {
	case callbackIndexer = <-callbackRan:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("same-repository mutation did not resume after IndexAll")
	}
	select {
	case <-lookupDone:
	case <-time.After(batchTransitionTestTimeout):
		t.Fatal("reach lookup did not resume after IndexAll publication")
	}
	currentA := mi.GetIndexer("repo-a")
	require.NotNil(t, currentA)
	require.NotSame(t, oldA, currentA)
	require.Same(t, currentA, callbackIndexer)
	require.Same(t, coordinatorA, currentA.attachedRepositoryMutationCoordinator())
	require.Greater(t, reach.BuildCounter(), beforeBuild)
}

type countingIndexAllStore struct {
	graph.Store
	mu      sync.Mutex
	batches int
}

func (s *countingIndexAllStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	s.mu.Lock()
	s.batches++
	s.mu.Unlock()
	s.Store.AddBatch(nodes, edges)
}

func (s *countingIndexAllStore) batchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.batches
}

func TestIndexAllDeduplicatesIdenticalRepositoryEntries(t *testing.T) {
	run := func(entries int) int {
		t.Helper()
		root := t.TempDir()
		writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Existing() {}\n")
		reg := parser.NewRegistry()
		reg.Register(languages.NewGoExtractor())
		cm := newTestConfigManager(t)
		for i := 0; i < entries; i++ {
			cm.Global().Repos = append(cm.Global().Repos, config.RepoEntry{Path: root, Name: "repo"})
		}
		store := &countingIndexAllStore{Store: graph.New()}
		mi := NewMultiIndexer(store, reg, search.NewBM25(), cm, zap.NewNop())
		results, err := mi.IndexAll()
		require.NoError(t, err)
		require.Len(t, results, 1)
		require.Same(t, mi.repositoryMutationCoordinator("repo"), mi.GetIndexer("repo").attachedRepositoryMutationCoordinator())
		return store.batchCount()
	}

	one := run(1)
	twoIdentical := run(2)
	require.Positive(t, one)
	require.Equal(t, one, twoIdentical, "a duplicate config entry must not launch a second raw worker")
}

func TestTrackRepoRechecksSamePathAcrossDifferentPrefixLanes(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "main.go"), "package sample\nfunc Existing() {}\n")
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	mi := NewMultiIndexer(graph.New(), reg, search.NewBM25(), newTestConfigManager(t), zap.NewNop())

	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	mi.SetOnRepoTracked(func(string, string) {
		arrived <- struct{}{}
		<-release
	})

	type trackOutcome struct {
		result *IndexResult
		err    error
	}
	outcomes := make(chan trackOutcome, 2)
	for _, name := range []string{"first", "second"} {
		name := name
		go func() {
			result, err := mi.TrackRepoCtx(context.Background(), config.RepoEntry{Path: root, Name: name})
			outcomes <- trackOutcome{result: result, err: err}
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-arrived:
		case <-time.After(batchTransitionTestTimeout):
			t.Fatal("concurrent track did not reach the cross-prefix admission barrier")
		}
	}
	close(release)

	nonNilResults := 0
	for i := 0; i < 2; i++ {
		select {
		case outcome := <-outcomes:
			require.NoError(t, outcome.err)
			if outcome.result != nil {
				nonNilResults++
			}
		case <-time.After(batchTransitionTestTimeout):
			t.Fatal("concurrent track did not complete")
		}
	}
	require.Equal(t, 1, nonNilResults)
	require.Len(t, mi.AllMetadata(), 1, "one directory must not be registered under two prefix lanes")
}
