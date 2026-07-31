package indexer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	"github.com/zzet/gortex/internal/parser/languages"
)

func TestWatcherModifyPublishesOnlyForMtimeOwner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Original() {}\n")

	g := graph.New()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(g, registry, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	w, err := NewWatcher(idx, config.WatchConfig{DebounceMs: 1}, zap.NewNop())
	require.NoError(t, err)
	type callback struct {
		oldNames []string
		newNames []string
	}
	var callbacks []callback
	w.OnSymbolChange(func(_ string, oldSymbols, newSymbols []*graph.Node) {
		callbacks = append(callbacks, callback{
			oldNames: watcherSymbolNames(oldSymbols),
			newNames: watcherSymbolNames(newSymbols),
		})
	})

	// A late startup replay owns neither the indexed file state nor a callback.
	_ = w.patchGraph(path, ChangeModified)
	require.Empty(t, callbacks)
	requireNoWatcherEvent(t, w)

	initial := indexedFileMtime(t, idx, path)
	writeTestFile(t, path, "package main\n\nfunc Modified() {}\n")
	// Equal mtimes are ambiguous: editors can preserve them and coarse
	// filesystems can collapse two writes into one tick. The persisted BLAKE3
	// receipt, not timestamp equality, must admit the changed bytes.
	setWatcherTestMtime(t, path, initial)
	_ = w.patchGraph(path, ChangeModified)
	require.Equal(t, []callback{{oldNames: []string{"Original"}, newNames: []string{"Modified"}}}, callbacks)
	event := waitForEvent(t, w, time.Second)
	require.Equal(t, "structural", event.Classification)

	// A duplicate native/poller report after commit must not replace the owning
	// callback with a Modified -> Modified replay.
	_ = w.patchGraph(path, ChangeModified)
	require.Len(t, callbacks, 1)
	requireNoWatcherEvent(t, w)

	// A byte-only edit is graph-inert but is still a real file mutation. It must
	// retain the existing inert callback/event semantics before its receipt is
	// allowed to suppress later duplicates.
	writeTestFile(t, path, "package main\n\nfunc Modified() {}\n\n")
	setWatcherTestMtime(t, path, initial)
	_ = w.patchGraph(path, ChangeModified)
	require.Len(t, callbacks, 2)
	require.Equal(t, []string{"Modified"}, callbacks[1].oldNames)
	require.Equal(t, []string{"Modified"}, callbacks[1].newNames)
	event = waitForEvent(t, w, time.Second)
	require.Contains(t, []string{"inert", "metadata_only"}, event.Classification)

	_ = w.patchGraph(path, ChangeModified)
	require.Len(t, callbacks, 2)
	requireNoWatcherEvent(t, w)
}

func TestWatcherInertModifyDoesNotStampWriteAfterProbe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "main.go")
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n")

	g := graph.New()
	registry := parser.NewRegistry()
	registry.Register(languages.NewGoExtractor())
	cfg := config.Default()
	cfg.Index.Workers = 1
	idx := New(g, registry, cfg.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)

	initial := indexedFileMtime(t, idx, path)
	firstMtime := initial.Add(time.Second)
	secondMtime := initial.Add(2 * time.Second)
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n\n")
	require.NoError(t, os.Chtimes(path, firstMtime, firstMtime))
	probe, ok := idx.prepareFileDelta(path)
	require.True(t, ok)
	require.True(t, probe.readVersion.valid)
	idx.discardPreparedExtraction(path)

	// The inert comparison owns the first save. A second save that lands before
	// receipt publication must remain dirty, while the first save still retains
	// its intentional zero-delta event/callback.
	writeTestFile(t, path, "package main\n\nfunc Value() int { return 0 }\n\n\n")
	require.NoError(t, os.Chtimes(path, secondMtime, secondMtime))
	fresh := idx.recordFileReadVersion(idx.relKey(path), path, probe.readVersion)
	require.False(t, fresh)
	require.NotEqual(t, secondMtime.UnixNano(), idx.FileMtimes()[idx.relKey(path)])
}

func watcherSymbolNames(nodes []*graph.Node) []string {
	names := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if node != nil && node.Kind != graph.KindFile && node.Kind != graph.KindImport {
			names = append(names, node.Name)
		}
	}
	return names
}

func indexedFileMtime(t *testing.T, idx *Indexer, path string) time.Time {
	t.Helper()
	mtime, ok := idx.FileMtimes()[idx.relKey(path)]
	require.True(t, ok)
	return time.Unix(0, mtime)
}

func setWatcherTestMtime(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	require.NoError(t, os.Chtimes(path, mtime, mtime))
}

func requireNoWatcherEvent(t *testing.T, w *Watcher) {
	t.Helper()
	select {
	case event := <-w.Events():
		t.Fatalf("unexpected watcher event: %+v", event)
	default:
	}
}
