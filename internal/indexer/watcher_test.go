package indexer

import (
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
)

func setupWatcher(t *testing.T) (string, *Indexer, *Watcher) {
	t.Helper()
	dir := t.TempDir()

	writeTestFile(t, filepath.Join(dir, "main.go"), `package main

func Original() {}
`)

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1

	idx := New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	wcfg := config.WatchConfig{
		Enabled:    true,
		Paths:      []string{dir},
		DebounceMs: 50, // short debounce for tests
		Exclude:    []string{"**/*.tmp", "**/.git/**"},
	}

	w, err := NewWatcher(idx, wcfg, zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, w.Start([]string{dir}))

	t.Cleanup(func() { _ = w.Stop() })
	return dir, idx, w
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func waitForEvent(t *testing.T, w *Watcher, timeout time.Duration) GraphChangeEvent {
	t.Helper()
	select {
	case ev := <-w.Events():
		return ev
	case <-time.After(timeout):
		w.mu.Lock()
		nextGeneration := w.nextGeneration
		pending := len(w.pending)
		pendingGenerations := len(w.pendingGeneration)
		waiters := len(w.mutationWaiters)
		w.mu.Unlock()
		t.Fatalf("timeout waiting for watcher event: next_generation=%d pending=%d pending_generations=%d waiters=%d history=%d",
			nextGeneration, pending, pendingGenerations, waiters, len(w.History()))
		return GraphChangeEvent{}
	}
}

func TestWatcher_OverflowReconcileUsesBatchCoordinator(t *testing.T) {
	_, _, w := setupWatcher(t)
	called := make(chan []string, 1)
	w.batchReindex = func(paths []string) (*IndexResult, error) {
		called <- paths
		return &IndexResult{}, nil
	}

	w.triggerOverflowReconcile("test")
	select {
	case paths := <-called:
		assert.Nil(t, paths, "overflow must request a full-tree batch reconcile")
	case <-time.After(2 * time.Second):
		t.Fatal("overflow reconcile did not invoke the batch coordinator")
	}
	w.asyncWork.Wait()
}

func TestWatcher_FileModify(t *testing.T) {
	dir, idx, w := setupWatcher(t)

	require.NotEmpty(t, idx.graph.FindNodesByName("Original"))

	// Modify the file.
	writeTestFile(t, filepath.Join(dir, "main.go"), `package main

func Modified() {}
`)

	ev := waitForEvent(t, w, 2*time.Second)
	assert.Equal(t, ChangeModified, ev.Kind)

	// Graph should reflect the change.
	assert.Empty(t, idx.graph.FindNodesByName("Original"))
	assert.NotEmpty(t, idx.graph.FindNodesByName("Modified"))
}

func TestWatcher_FileCreate(t *testing.T) {
	dir, idx, w := setupWatcher(t)

	nodesBefore := idx.graph.NodeCount()

	writeTestFile(t, filepath.Join(dir, "new.go"), `package main

func NewFunc() {}
`)

	ev := waitForEvent(t, w, 2*time.Second)
	// fsnotify may emit CREATE or WRITE depending on the OS.
	assert.Contains(t, []ChangeKind{ChangeCreated, ChangeModified}, ev.Kind)
	assert.Greater(t, idx.graph.NodeCount(), nodesBefore)
	assert.NotEmpty(t, idx.graph.FindNodesByName("NewFunc"))
}

func TestWatcher_FileDelete(t *testing.T) {
	dir, idx, w := setupWatcher(t)

	require.NotEmpty(t, idx.graph.FindNodesByName("Original"))

	require.NoError(t, os.Remove(filepath.Join(dir, "main.go")))

	ev := waitForEvent(t, w, 2*time.Second)
	assert.Equal(t, ChangeDeleted, ev.Kind)
	assert.Empty(t, idx.graph.FindNodesByName("Original"))
}

func TestWatcher_History(t *testing.T) {
	dir, _, w := setupWatcher(t)

	writeTestFile(t, filepath.Join(dir, "main.go"), `package main

func Changed() {}
`)
	_ = waitForEvent(t, w, 2*time.Second)

	history := w.History()
	require.Len(t, history, 1)
	assert.Equal(t, ChangeModified, history[0].Kind)
}

func TestWatcher_SymbolChangeCallback_Modify(t *testing.T) {
	dir, _, w := setupWatcher(t)

	type callbackData struct {
		filePath   string
		oldSymbols []*graph.Node
		newSymbols []*graph.Node
	}

	var mu sync.Mutex
	var calls []callbackData

	w.OnSymbolChange(func(filePath string, oldSymbols, newSymbols []*graph.Node) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, callbackData{filePath, oldSymbols, newSymbols})
	})

	// Modify the file — changes function name.
	writeTestFile(t, filepath.Join(dir, "main.go"), `package main

func Modified() {}
`)
	_ = waitForEvent(t, w, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 1)

	// Old symbols should contain "Original", new should contain "Modified".
	var oldNames, newNames []string
	for _, n := range calls[0].oldSymbols {
		oldNames = append(oldNames, n.Name)
	}
	for _, n := range calls[0].newSymbols {
		if n.Kind != graph.KindFile && n.Kind != graph.KindImport {
			newNames = append(newNames, n.Name)
		}
	}
	assert.Contains(t, oldNames, "Original")
	assert.Contains(t, newNames, "Modified")
}

func TestWatcher_SymbolChangeCallback_Delete(t *testing.T) {
	dir, _, w := setupWatcher(t)
	require.NotEmpty(t, w.snapshotSymbols("main.go"), "setup must index Original before watching deletion")

	type callbackData struct {
		filePath   string
		oldSymbols []*graph.Node
		newSymbols []*graph.Node
	}

	var mu sync.Mutex
	var calls []callbackData

	w.OnSymbolChange(func(filePath string, oldSymbols, newSymbols []*graph.Node) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, callbackData{filePath, oldSymbols, newSymbols})
	})

	require.NoError(t, os.Remove(filepath.Join(dir, "main.go")))
	_ = waitForEvent(t, w, 2*time.Second)

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, calls, 1)

	// Old symbols should have entries, new should be nil (deleted).
	assert.NotEmpty(t, calls[0].oldSymbols, "callback=%+v", calls[0])
	assert.Nil(t, calls[0].newSymbols)
}

func TestWatcher_DirScanDeleteOverlapPublishesPrimaryOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, `package main

func Original() {}
`)

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(g, reg, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 50}, zap.NewNop())
	require.NoError(t, err)

	type callbackData struct {
		oldSymbols []*graph.Node
		newSymbols []*graph.Node
	}
	var calls []callbackData
	w.OnSymbolChange(func(_ string, oldSymbols, newSymbols []*graph.Node) {
		calls = append(calls, callbackData{oldSymbols: oldSymbols, newSymbols: newSymbols})
	})

	// Force the bad ordering deterministically: the file disappears, a
	// new-directory discovery scan observes that absence first, and the real
	// file event is delivered only afterward. Discovery must not consume the
	// deletion or its pre-delete symbol snapshot.
	require.NoError(t, os.Remove(path))
	w.runDirScan(map[string]struct{}{dir: {}}, nil)
	require.NotEmpty(t, g.FindNodesByName("Original"))

	_ = w.patchGraph(path, ChangeDeleted)
	_ = w.patchGraph(path, ChangeDeleted) // redundant producer is suppressed

	require.Len(t, calls, 1)
	require.NotEmpty(t, calls[0].oldSymbols)
	oldNames := make([]string, 0, len(calls[0].oldSymbols))
	for _, symbol := range calls[0].oldSymbols {
		oldNames = append(oldNames, symbol.Name)
	}
	assert.Contains(t, oldNames, "Original")
	assert.Nil(t, calls[0].newSymbols)

	select {
	case ev := <-w.Events():
		assert.Equal(t, ChangeDeleted, ev.Kind)
		assert.Positive(t, ev.NodesRemoved)
	default:
		t.Fatal("primary delete event was not published")
	}
	select {
	case ev := <-w.Events():
		t.Fatalf("duplicate delete event published: %+v", ev)
	default:
	}
}
