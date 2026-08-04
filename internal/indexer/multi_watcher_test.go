package indexer

import (
	"os"
	"path/filepath"
	"strings"
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
	"github.com/zzet/gortex/internal/search"
)

// setupMultiWatcherTest creates a MultiIndexer with two repos, indexes them,
// and returns the MultiWatcher, repo dirs, and cleanup function.
func setupMultiWatcherTest(t *testing.T) (*MultiWatcher, *MultiIndexer, string, string) {
	t.Helper()

	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cm := newTestConfigManager(t)

	// Create two repo directories.
	repoADir := filepath.Join(t.TempDir(), "repo-a")
	repoBDir := filepath.Join(t.TempDir(), "repo-b")
	require.NoError(t, os.MkdirAll(repoADir, 0o755))
	require.NoError(t, os.MkdirAll(repoBDir, 0o755))

	writeFile(t, filepath.Join(repoADir, "main.go"), `package main

func HelloA() {}
`)
	writeFile(t, filepath.Join(repoBDir, "main.go"), `package main

func HelloB() {}
`)

	// Add repos to config manager so ActiveRepos returns them.
	cm.Global().Repos = []config.RepoEntry{
		{Path: repoADir, Name: "repo-a"},
		{Path: repoBDir, Name: "repo-b"},
	}

	mi := NewMultiIndexer(g, reg, search.NewAuto(), cm, zap.NewNop())
	_, err := mi.IndexAll()
	require.NoError(t, err)

	configs := map[string]config.WatchConfig{
		"repo-a": {
			Enabled:    true,
			DebounceMs: 50,
			Exclude:    []string{"**/*.tmp"},
		},
		"repo-b": {
			Enabled:    true,
			DebounceMs: 50,
			Exclude:    []string{"**/*.tmp"},
		},
	}

	mw, err := NewMultiWatcher(mi, configs, zap.NewNop())
	require.NoError(t, err)

	return mw, mi, repoADir, repoBDir
}

func waitForMultiEvent(t *testing.T, mw *MultiWatcher, timeout time.Duration) GraphChangeEvent {
	t.Helper()
	select {
	case ev := <-mw.Events():
		return ev
	case <-time.After(timeout):
		t.Fatal("timeout waiting for multi-watcher event")
		return GraphChangeEvent{}
	}
}

func TestNewMultiWatcher(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	defer func() { _ = mw.Stop() }()

	// Should have watchers for both repos.
	assert.Len(t, mw.watchers, 2)
	assert.Contains(t, mw.watchers, "repo-a")
	assert.Contains(t, mw.watchers, "repo-b")
}

func TestNewMultiWatcher_InaccessibleRepo(t *testing.T) {
	g := graph.New()
	reg := parser.NewRegistry()
	reg.Register(languages.NewGoExtractor())
	cm := newTestConfigManager(t)

	// Create one valid repo.
	repoDir := filepath.Join(t.TempDir(), "valid-repo")
	require.NoError(t, os.MkdirAll(repoDir, 0o755))
	writeFile(t, filepath.Join(repoDir, "main.go"), `package main
func Hello() {}
`)

	cm.Global().Repos = []config.RepoEntry{
		{Path: repoDir, Name: "valid"},
	}

	mi := NewMultiIndexer(g, reg, search.NewAuto(), cm, zap.NewNop())
	_, err := mi.IndexAll()
	require.NoError(t, err)

	// Include a config for a non-existent repo prefix — should log warning and continue.
	configs := map[string]config.WatchConfig{
		"valid": {
			Enabled:    true,
			DebounceMs: 50,
		},
		"nonexistent": {
			Enabled:    true,
			DebounceMs: 50,
		},
	}

	mw, err := NewMultiWatcher(mi, configs, zap.NewNop())
	require.NoError(t, err)
	defer func() { _ = mw.Stop() }()

	// Only the valid repo should have a watcher.
	assert.Len(t, mw.watchers, 1)
	assert.Contains(t, mw.watchers, "valid")
}

func TestMultiWatcher_StartStop(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)

	require.NoError(t, mw.Start())
	require.NoError(t, mw.Stop())
}

func TestMultiWatcher_Events_FileModify(t *testing.T) {
	mw, mi, repoADir, _ := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())
	defer func() { _ = mw.Stop() }()

	// Verify initial state.
	require.NotEmpty(t, mi.Graph().FindNodesByName("HelloA"))

	// Modify a file in repo-a.
	writeFile(t, filepath.Join(repoADir, "main.go"), `package main

func ModifiedA() {}
`)

	ev := waitForMultiEvent(t, mw, 3*time.Second)
	assert.Equal(t, ChangeModified, ev.Kind)

	// Graph should reflect the change. The reindex runs asynchronously after
	// the change event, so poll rather than assert immediately.
	require.Eventually(t, func() bool {
		return len(mi.Graph().FindNodesByName("ModifiedA")) > 0
	}, 3*time.Second, 20*time.Millisecond, "ModifiedA should be indexed")
}

func TestMultiWatcher_Events_MergedFromMultipleRepos(t *testing.T) {
	mw, mi, repoADir, repoBDir := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())
	defer func() { _ = mw.Stop() }()

	// Modify repo-a.
	writeFile(t, filepath.Join(repoADir, "main.go"), `package main

func ChangedA() {}
`)
	_ = waitForMultiEvent(t, mw, 3*time.Second)

	// Modify repo-b.
	writeFile(t, filepath.Join(repoBDir, "main.go"), `package main

func ChangedB() {}
`)
	_ = waitForMultiEvent(t, mw, 3*time.Second)

	// Both changes should be reflected in the graph. The change event fires
	// when the modification is DETECTED; the reindex that materialises the new
	// symbol runs asynchronously after, so poll for the nodes rather than
	// asserting immediately — the fixed-instant assert raced the reindex and
	// flaked under CI load.
	require.Eventually(t, func() bool {
		return len(mi.Graph().FindNodesByName("ChangedA")) > 0 &&
			len(mi.Graph().FindNodesByName("ChangedB")) > 0
	}, 3*time.Second, 20*time.Millisecond, "both ChangedA and ChangedB should be indexed")
}

func TestMultiWatcher_AddRepo(t *testing.T) {
	mw, mi, _, _ := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())
	defer func() { _ = mw.Stop() }()

	// Create a new repo directory.
	newRepoDir := filepath.Join(t.TempDir(), "repo-c")
	require.NoError(t, os.MkdirAll(newRepoDir, 0o755))
	writeFile(t, filepath.Join(newRepoDir, "main.go"), `package main
func HelloC() {}
`)

	// Track the new repo in the multi-indexer first.
	_, err := mi.TrackRepo(config.RepoEntry{Path: newRepoDir, Name: "repo-c"})
	require.NoError(t, err)

	// Add watcher for the new repo.
	err = mw.AddRepo("repo-c", config.WatchConfig{
		Enabled:    true,
		DebounceMs: 50,
	})
	require.NoError(t, err)

	assert.Contains(t, mw.watchers, "repo-c")

	// Modify the new repo and verify events flow.
	writeFile(t, filepath.Join(newRepoDir, "main.go"), `package main
func ModifiedC() {}
`)
	ev := waitForMultiEvent(t, mw, 3*time.Second)
	assert.Equal(t, ChangeModified, ev.Kind)
	assert.NotEmpty(t, mi.Graph().FindNodesByName("ModifiedC"))
}

func TestMultiWatcher_AddRepo_AlreadyExists(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	defer func() { _ = mw.Stop() }()

	err := mw.AddRepo("repo-a", config.WatchConfig{Enabled: true, DebounceMs: 50})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")
}

func TestMultiWatcher_RemoveRepo(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())

	err := mw.RemoveRepo("repo-a")
	require.NoError(t, err)
	assert.NotContains(t, mw.watchers, "repo-a")
	assert.Contains(t, mw.watchers, "repo-b")

	// Cleanup remaining.
	_ = mw.Stop()
}

func TestMultiWatcher_RemoveRepo_NotFound(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	defer func() { _ = mw.Stop() }()

	err := mw.RemoveRepo("nonexistent")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no watcher")
}

func TestMultiWatcher_EventsChannel(t *testing.T) {
	mw, _, _, _ := setupMultiWatcherTest(t)
	defer func() { _ = mw.Stop() }()

	ch := mw.Events()
	assert.NotNil(t, ch)
}

// TestMultiWatcher_HistoryUnionAcrossRepos exercises the History /
// HistorySince surface MCP's get_recent_changes consumes. The
// MultiWatcher must merge per-repo histories newest-first so an agent
// gets a unified change feed for the whole workspace.
func TestMultiWatcher_HistoryUnionAcrossRepos(t *testing.T) {
	mw, mi, repoADir, repoBDir := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())
	defer func() { _ = mw.Stop() }()

	before := time.Now()

	writeFile(t, filepath.Join(repoADir, "main.go"), `package main
func ChangedA() {}
`)
	_ = waitForMultiEvent(t, mw, 3*time.Second)

	writeFile(t, filepath.Join(repoBDir, "main.go"), `package main
func ChangedB() {}
`)
	_ = waitForMultiEvent(t, mw, 3*time.Second)

	// Allow per-watcher history to settle (history is appended on the
	// same goroutine that emits the channel event so it's visible by
	// the time the event surfaces, but be conservative).
	time.Sleep(50 * time.Millisecond)

	hist := mw.History()
	require.NotEmpty(t, hist, "history should contain both repo edits")

	// At minimum: one event per repo from each modify burst.
	//
	// FilePath is absolute, so the merged feed carries no attribution of
	// its own — RepoPrefix is what lets a consumer tell which repo (and so
	// which workspace) a row belongs to, and it must agree with the root
	// the path falls under.
	repoSeen := map[string]bool{}
	for _, ev := range hist {
		if strings.HasPrefix(ev.FilePath, repoADir) {
			repoSeen["a"] = true
			assert.Equal(t, "repo-a", ev.RepoPrefix, "repo-a event must be stamped with its repo prefix")
		}
		if strings.HasPrefix(ev.FilePath, repoBDir) {
			repoSeen["b"] = true
			assert.Equal(t, "repo-b", ev.RepoPrefix, "repo-b event must be stamped with its repo prefix")
		}
	}
	assert.True(t, repoSeen["a"], "history must include repo-a event")
	assert.True(t, repoSeen["b"], "history must include repo-b event")

	// Newest-first ordering.
	for i := 1; i < len(hist); i++ {
		assert.False(t, hist[i].Timestamp.After(hist[i-1].Timestamp),
			"history must be sorted newest-first")
	}

	// HistorySince filters by timestamp; everything we observed is
	// after `before`.
	since := mw.HistorySince(before)
	assert.Equal(t, len(hist), len(since),
		"HistorySince(before-everything) should equal History()")

	// A future cutoff yields nothing.
	future := mw.HistorySince(time.Now().Add(time.Hour))
	assert.Empty(t, future)

	_ = mi // silence unused
}

// TestMultiWatcher_OnSymbolChangeFanout exercises the OnSymbolChange
// surface MCP's symbolHistory consumes. The callback must:
//   - fan out to every per-repo Watcher present at registration time
//   - be applied to repos added at runtime via AddRepo
//
// Per-Watcher emits relative paths (e.g. "main.go") so we count
// invocations per per-Watcher rather than match absolute prefixes.
func TestMultiWatcher_OnSymbolChangeFanout(t *testing.T) {
	mw, mi, repoADir, repoBDir := setupMultiWatcherTest(t)
	require.NoError(t, mw.Start())
	defer func() { _ = mw.Stop() }()

	var mu sync.Mutex
	var hits int
	mw.OnSymbolChange(func(filePath string, oldSymbols, newSymbols []*graph.Node) {
		mu.Lock()
		hits++
		mu.Unlock()
	})

	// Verify both registration-time watchers got the callback wired.
	mw.mu.Lock()
	wA := mw.watchers["repo-a"]
	wB := mw.watchers["repo-b"]
	mw.mu.Unlock()
	require.NotNil(t, wA)
	require.NotNil(t, wB)
	wA.symbolChangeCbMu.RLock()
	require.NotNil(t, wA.symbolChangeCb, "repo-a watcher must have callback after MultiWatcher.OnSymbolChange")
	wA.symbolChangeCbMu.RUnlock()
	wB.symbolChangeCbMu.RLock()
	require.NotNil(t, wB.symbolChangeCb, "repo-b watcher must have callback after MultiWatcher.OnSymbolChange")
	wB.symbolChangeCbMu.RUnlock()

	// Modify the two existing repos.
	writeFile(t, filepath.Join(repoADir, "main.go"), `package main
func ChangedA() {}
`)
	_ = waitForMultiEvent(t, mw, 3*time.Second)

	writeFile(t, filepath.Join(repoBDir, "main.go"), `package main
func ChangedB() {}
`)
	_ = waitForMultiEvent(t, mw, 3*time.Second)

	// Add a repo at runtime and modify it — the callback must apply
	// without a second OnSymbolChange call.
	newRepoDir := filepath.Join(t.TempDir(), "repo-c")
	require.NoError(t, os.MkdirAll(newRepoDir, 0o755))
	writeFile(t, filepath.Join(newRepoDir, "main.go"), `package main
func HelloC() {}
`)
	_, err := mi.TrackRepo(config.RepoEntry{Path: newRepoDir, Name: "repo-c"})
	require.NoError(t, err)
	require.NoError(t, mw.AddRepo("repo-c", config.WatchConfig{
		Enabled: true, DebounceMs: 50,
	}))

	// Verify AddRepo wired the callback onto the new repo's per-Watcher.
	mw.mu.Lock()
	wC := mw.watchers["repo-c"]
	mw.mu.Unlock()
	require.NotNil(t, wC)
	wC.symbolChangeCbMu.RLock()
	require.NotNil(t, wC.symbolChangeCb,
		"AddRepo must propagate the symbol-change callback to the new per-Watcher")
	wC.symbolChangeCbMu.RUnlock()

	writeFile(t, filepath.Join(newRepoDir, "main.go"), `package main
func ChangedC() {}
`)
	_ = waitForMultiEvent(t, mw, 3*time.Second)

	// Allow callbacks to settle.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, hits, 3,
		"callback should fire at least once per modify across the 3 repos (got %d)", hits)
}
