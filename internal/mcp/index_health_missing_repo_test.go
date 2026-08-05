package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
)

// Issue #312: a tracked repo whose directory was deleted kept its
// registration silently. Every other signal in index_health describes
// what IS in the graph, so none of them can notice a repo that stopped
// existing — the report stays green while workspace-wide answers quietly
// omit a whole repository.

// healthServerTrackingRepos returns a server whose config registry tracks
// the given paths, so index_health can audit their liveness.
func healthServerTrackingRepos(t *testing.T, paths ...string) *Server {
	t.Helper()
	srv, _ := setupTestServer(t)

	entries := make([]config.RepoEntry, 0, len(paths))
	for _, p := range paths {
		entries = append(entries, config.RepoEntry{Path: p})
	}
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: entries}
	gc.SetConfigPath(cfgPath)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(cfgPath)
	require.NoError(t, err)
	srv.configManager = cm
	return srv
}

func TestIndexHealth_FlagsTrackedRepoWithDeletedPath(t *testing.T) {
	live := t.TempDir()
	gone := filepath.Join(t.TempDir(), "deleted-repo")
	require.NoError(t, os.MkdirAll(gone, 0o755))
	require.NoError(t, os.RemoveAll(gone))

	srv := healthServerTrackingRepos(t, live, gone)

	payload := srv.buildIndexHealthPayload()
	require.NotNil(t, payload)

	assert.Equal(t, false, payload["tracked_repo_paths_ok"])
	assert.Equal(t, []string{gone}, payload["missing_repo_paths"],
		"only the deleted repo is reported")

	rec, _ := payload["recommendation"].(string)
	assert.Contains(t, rec, gone)
	assert.Contains(t, rec, "gortex untrack",
		"health must name the command that fixes the inventory")
}

func TestIndexHealth_AllRepoPathsLiveStaysGreen(t *testing.T) {
	srv := healthServerTrackingRepos(t, t.TempDir(), t.TempDir())

	payload := srv.buildIndexHealthPayload()
	require.NotNil(t, payload)

	assert.Equal(t, true, payload["tracked_repo_paths_ok"])
	assert.NotContains(t, payload, "missing_repo_paths")
	rec, _ := payload["recommendation"].(string)
	assert.NotContains(t, rec, "gortex untrack")
}

// The path liveness audit must survive a server with no config registry
// wired (an embedded single-repo server) rather than panicking or
// inventing a missing repo.
func TestIndexHealth_NoConfigRegistryReportsNoMissingRepos(t *testing.T) {
	srv, _ := setupTestServer(t)

	payload := srv.buildIndexHealthPayload()
	require.NotNil(t, payload)
	assert.Equal(t, true, payload["tracked_repo_paths_ok"])
	assert.Empty(t, srv.missingTrackedRepoPaths())
}
