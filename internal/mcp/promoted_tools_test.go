package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/indexer"
	"github.com/zzet/gortex/internal/query"
)

// TestPromotedToolsManager_PersistAndDemote unit-tests the learned surface:
// a promotion persists across "restarts" (fresh managers over the same
// store) and is demoted once it has gone unused past the hysteresis window.
func TestPromotedToolsManager_PersistAndDemote(t *testing.T) {
	cacheDir := tempSidecarDir(t)
	repo := filepath.Join(tempSidecarDir(t), "repo")

	// Session 1 (epoch 1): promote a deferred tool.
	m1 := newPromotedToolsManager(cacheDir, repo)
	require.Empty(t, m1.Load())
	m1.Record("get_architecture", "")
	require.True(t, m1.Has("get_architecture"))

	// Session 2 (epoch 2, restart): the promotion survives.
	require.Contains(t, newPromotedToolsManager(cacheDir, repo).Load(), "get_architecture")
	// Session 3 (epoch 3): still within the window (3-1 = 2 < 3).
	require.Contains(t, newPromotedToolsManager(cacheDir, repo).Load(), "get_architecture")
	// Session 4 (epoch 4): unused for 3 epochs → demoted.
	require.NotContains(t, newPromotedToolsManager(cacheDir, repo).Load(), "get_architecture")
	// And it stays gone.
	require.NotContains(t, newPromotedToolsManager(cacheDir, repo).Load(), "get_architecture")
}

// TestPromotedToolsManager_UseResetsClock: re-using a promotion resets its
// demotion clock, so it outlives what would otherwise be its window.
func TestPromotedToolsManager_UseResetsClock(t *testing.T) {
	cacheDir := tempSidecarDir(t)
	repo := filepath.Join(tempSidecarDir(t), "repo")

	// Promote at epoch 1, then re-use at epoch 2 (resets last_used to 2).
	m1 := newPromotedToolsManager(cacheDir, repo)
	m1.Load()
	m1.Record("taint_paths", "")
	m2 := newPromotedToolsManager(cacheDir, repo)
	m2.Load()
	m2.Record("taint_paths", "") // reset

	// Without the reset it would demote at epoch 4 (4-1=3). With it, it
	// survives to epoch 4 and only demotes at epoch 5 (5-2=3).
	require.Contains(t, newPromotedToolsManager(cacheDir, repo).Load(), "taint_paths")    // epoch 3
	require.Contains(t, newPromotedToolsManager(cacheDir, repo).Load(), "taint_paths")    // epoch 4
	require.NotContains(t, newPromotedToolsManager(cacheDir, repo).Load(), "taint_paths") // epoch 5
}

func buildLearnedServer(t *testing.T, cacheDir, dir string) *Server {
	t.Helper()
	g := graph.New()
	reg := testRegistry()
	conf := config.Default()
	idx := indexer.New(g, reg, conf.Index, zap.NewNop())
	_, err := idx.Index(dir)
	require.NoError(t, err)
	eng := query.NewEngine(g)
	cfg := ToolPolicyConfig{Preset: "agent", Mode: "defer"}
	srv := NewServer(eng, g, idx, nil, zap.NewNop(), nil, MultiRepoOptions{ToolPolicy: &cfg})
	srv.InitLearnedTools(cacheDir, dir)
	return srv
}

// TestLearnedSurface_SurvivesRestart: a deferred tool promoted (and used)
// in one session is re-promoted into the cold agent surface on the next
// server startup — the learned surface persists across restarts.
func TestLearnedSurface_SurvivesRestart(t *testing.T) {
	cacheDir := tempSidecarDir(t)
	dir := tempSidecarDir(t)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package app\n\nfunc Main() {}\n"), 0o644))

	// Session 1: get_architecture is deferred under the agent preset; a
	// direct call promotes it, and using it records the learned promotion.
	s1 := buildLearnedServer(t, cacheDir, dir)
	require.Equal(t, "deferred", s1.toolStatus("get_architecture"),
		"non-floor tool starts deferred under the agent preset")
	require.True(t, s1.EnsureToolPromoted("get_architecture"))
	s1.NoteToolUse("get_architecture", "", true)
	names1 := listToolNamesForSession(t, s1, "")
	require.True(t, names1["get_architecture"],
		"a learned tool stays visible on the lean agent surface")

	// Session 2 (restart): a fresh server re-promotes it from persistence,
	// so it is in the cold agent tools/list without any discovery hop.
	s2 := buildLearnedServer(t, cacheDir, dir)
	require.True(t, s2.isLearnedPromoted("get_architecture"))
	names2 := listToolNamesForSession(t, s2, "")
	require.True(t, names2["get_architecture"],
		"the learned tool is back in the cold surface after restart")
	require.Equal(t, 1, s2.LearnedToolCount())
}
