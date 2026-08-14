package indexer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
)

type blockingLifecycleExtractor struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (e *blockingLifecycleExtractor) Language() string     { return "blocking" }
func (e *blockingLifecycleExtractor) Extensions() []string { return []string{".block"} }
func (e *blockingLifecycleExtractor) Extract(string, []byte) (*parser.ExtractionResult, error) {
	e.once.Do(func() { close(e.started) })
	<-e.release
	return &parser.ExtractionResult{}, nil
}

func TestIndexerCloseWaitsForTimedOutExtraction(t *testing.T) {
	ext := &blockingLifecycleExtractor{started: make(chan struct{}), release: make(chan struct{})}
	reg := parser.NewRegistry()
	reg.Register(ext)
	idx := New(graph.New(), reg, config.IndexConfig{MaxExtractMillis: 1}, zap.NewNop())

	_, err := idx.extractWithTimeoutDone(ext, "slow.block", []byte("source"), nil)
	if !errors.Is(err, errExtractTimeout) {
		t.Fatalf("extract error = %v, want timeout", err)
	}
	select {
	case <-ext.started:
	case <-time.After(time.Second):
		t.Fatal("extractor did not start")
	}

	closed := make(chan struct{})
	go func() {
		idx.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned while timed-out extraction was still active")
	case <-time.After(25 * time.Millisecond):
	}
	close(ext.release)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after extraction released")
	}

	if _, err := idx.ExtractBuffer("blocking", "after.block", nil); !errors.Is(err, ErrIndexerClosed) {
		t.Fatalf("post-close extraction error = %v, want ErrIndexerClosed", err)
	}
	idx.Close()
}

func TestMultiIndexerCloseDrainsMutationAndClosesIndexers(t *testing.T) {
	reg := parser.NewRegistry()
	ext := &blockingLifecycleExtractor{started: make(chan struct{}), release: make(chan struct{})}
	reg.Register(ext)
	idx := New(graph.New(), reg, config.IndexConfig{}, zap.NewNop())

	coordinator := newRepositoryMutationCoordinator(nil)
	mi := &MultiIndexer{
		indexers:            map[string]*Indexer{"repo": idx},
		repos:               map[string]*RepoMetadata{"repo": {RepoPrefix: "repo"}},
		repositoryMutations: map[string]*repositoryMutationCoordinator{"repo": coordinator},
	}
	mutationStarted := make(chan struct{})
	mutationRelease := make(chan struct{})
	mutationDone := make(chan error, 1)
	go func() {
		mutationDone <- coordinator.runExclusive(context.Background(), func() error {
			close(mutationStarted)
			<-mutationRelease
			return nil
		})
	}()
	<-mutationStarted

	closeDone := make(chan error, 1)
	go func() { closeDone <- mi.Close(context.Background()) }()
	deadline := time.After(time.Second)
	for !mi.isClosed() {
		select {
		case <-deadline:
			t.Fatal("MultiIndexer did not close mutation admission")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	if err := mi.repositoryMutationCoordinator("new").runExclusive(context.Background(), func() error { return nil }); !errors.Is(err, errRepositoryMutationCoordinatorClosed) {
		t.Fatalf("new mutation error = %v", err)
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before mutation drained: %v", err)
	case <-time.After(25 * time.Millisecond):
	}

	close(mutationRelease)
	if err := <-mutationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closeDone; err != nil {
		t.Fatal(err)
	}
	if got := mi.GetIndexer("repo"); got != nil {
		t.Fatalf("detached indexer = %#v", got)
	}
	if _, err := idx.ExtractBuffer("blocking", "after.block", nil); !errors.Is(err, ErrIndexerClosed) {
		t.Fatalf("owned indexer was not closed: %v", err)
	}
	if err := mi.Close(context.Background()); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
}
