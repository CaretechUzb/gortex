package indexer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/reach"
	"github.com/zzet/gortex/internal/search"
	"go.uber.org/zap"
)

type topologyBatchBlockingExtractor struct {
	entered chan<- string
	release <-chan struct{}
}

func (e *topologyBatchBlockingExtractor) Language() string     { return "topology-batch-test" }
func (e *topologyBatchBlockingExtractor) Extensions() []string { return []string{".topobatch"} }
func (e *topologyBatchBlockingExtractor) Extract(filePath string, _ []byte) (*parser.ExtractionResult, error) {
	e.entered <- filePath
	<-e.release
	return &parser.ExtractionResult{}, nil
}

func TestRunRepositoryTopologyBatchAllowsParallelTrackAndBlocksReach(t *testing.T) {
	root := t.TempDir()
	repoPaths := []string{filepath.Join(root, "repo-a"), filepath.Join(root, "repo-b")}
	for _, repoPath := range repoPaths {
		if err := os.MkdirAll(repoPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repoPath, "source.topobatch"), []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	configManager, err := config.NewConfigManager(filepath.Join(root, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan string, len(repoPaths))
	release := make(chan struct{})
	releaseWorkers := sync.OnceFunc(func() { close(release) })
	defer releaseWorkers()
	registry := parser.NewRegistry()
	registry.Register(&topologyBatchBlockingExtractor{entered: entered, release: release})
	store := graph.New()
	mi := NewMultiIndexer(store, registry, search.NewBM25(), configManager, zap.NewNop())
	mi.BeginParallelBatch()
	defer mi.ResetBatch()

	beforeGeneration := reach.BuildCounter()
	lookupDone := make(chan struct{})
	err = mi.RunRepositoryTopologyBatch(context.Background(), func(batchCtx context.Context) error {
		var workers sync.WaitGroup
		trackErrors := make(chan error, len(repoPaths))
		for _, repoPath := range repoPaths {
			repoPath := repoPath
			workers.Add(1)
			go func() {
				defer workers.Done()
				_, trackErr := mi.TrackRepoCtx(batchCtx, config.RepoEntry{Path: repoPath})
				trackErrors <- trackErr
			}()
		}

		for range repoPaths {
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				releaseWorkers()
				workers.Wait()
				return fmt.Errorf("repository parsers did not overlap inside topology batch")
			}
		}

		go func() {
			reach.Lookup(store, "missing")
			close(lookupDone)
		}()
		select {
		case <-lookupDone:
			releaseWorkers()
			workers.Wait()
			return fmt.Errorf("reach lookup crossed active topology batch")
		case <-time.After(25 * time.Millisecond):
		}

		releaseWorkers()
		workers.Wait()
		close(trackErrors)
		for trackErr := range trackErrors {
			if trackErr != nil {
				return trackErr
			}
		}
		if got := reach.BuildCounter(); got != beforeGeneration {
			return fmt.Errorf("topology generation published before batch join: got %d want %d", got, beforeGeneration)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-lookupDone:
	case <-time.After(2 * time.Second):
		t.Fatal("reach lookup remained blocked after topology batch")
	}
	if got, want := reach.BuildCounter(), beforeGeneration+1; got != want {
		t.Fatalf("topology generation advances = %d, want %d", got, want)
	}
	if got := len(mi.AllMetadata()); got != len(repoPaths) {
		t.Fatalf("tracked repositories = %d, want %d", got, len(repoPaths))
	}
}

func TestRunRepositoryTopologyBatchReleasesOnEveryExitAndRejectsStaleContext(t *testing.T) {
	mi := &MultiIndexer{graph: graph.New()}
	beforeGeneration := reach.BuildCounter()
	sentinel := fmt.Errorf("stop batch")
	var escaped context.Context
	err := mi.RunRepositoryTopologyBatch(context.Background(), func(batchCtx context.Context) error {
		escaped = batchCtx
		return sentinel
	})
	if err != sentinel {
		t.Fatalf("error = %v, want %v", err, sentinel)
	}
	if got := reach.BuildCounter(); got != beforeGeneration {
		t.Fatalf("no-op error batch advanced topology generation: got %d want %d", got, beforeGeneration)
	}
	if activeRepositoryTopologyBatch(escaped, mi) != nil {
		t.Fatal("context remained topology-batch active after callback")
	}

	other := &MultiIndexer{graph: graph.New()}
	err = mi.RunRepositoryTopologyBatch(context.Background(), func(batchCtx context.Context) error {
		return other.RunRepositoryTopologyBatch(batchCtx, func(context.Context) error { return nil })
	})
	if err != errRepositoryTopologyBatchOwnerMismatch {
		t.Fatalf("cross-owner nested batch error = %v, want %v", err, errRepositoryTopologyBatchOwnerMismatch)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err = mi.RunRepositoryTopologyBatch(canceled, func(context.Context) error {
		called = true
		return nil
	})
	if err != context.Canceled || called {
		t.Fatalf("pre-canceled batch: err=%v called=%v", err, called)
	}

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("batch callback panic was not propagated")
			}
		}()
		_ = mi.RunRepositoryTopologyBatch(context.Background(), func(context.Context) error {
			panic("topology batch test panic")
		})
	}()

	done := make(chan struct{})
	go func() {
		finish := mi.beginRepositoryTopologyMutation(escaped)
		finish(false)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ordinary topology mutation remained blocked after batch exit")
	}
}
