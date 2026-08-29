package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGetRepoConfig_PartialWorkspaceConfigKeepsDefaults: a .gortex.yaml
// that sets only what it cares about must not zero every other field.
// Before the fix, the file's mere presence replaced Default() wholesale:
// unset index.workers collapsed the parse pool to a single core (a
// silent ~12× cold-index slowdown) and unset max_parse_bytes_in_flight
// disabled the parse-admission semaphore outright.
func TestGetRepoConfig_PartialWorkspaceConfigKeepsDefaults(t *testing.T) {
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)

	repoDir := t.TempDir()
	wsContent := `
index:
  exclude:
    - "**/[Bb]in/**"
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("my-repo", repoDir)

	cfg := cm.GetRepoConfig("my-repo")
	require.NotNil(t, cfg)

	def := Default()
	assert.Equal(t, runtime.NumCPU(), cfg.Index.Workers,
		"unset index.workers must keep the NumCPU default, not collapse to 0→1")
	assert.Equal(t, def.Index.MaxParseBytesInFlight, cfg.Index.MaxParseBytesInFlight,
		"unset max_parse_bytes_in_flight must keep the admission budget, not disable it")
	assert.Equal(t, def.Index.Content.MaxDocumentBytes, cfg.Index.Content.MaxDocumentBytes,
		"content-admission caps must survive a partial workspace config")
	// The file's own settings still land.
	assert.Contains(t, cfg.Index.Exclude, "**/[Bb]in/**")
	assert.True(t, cfg.Index.AllowedFrameworks().Allows("django"),
		"an unset index.frameworks block must allow every framework")
}

// TestGetRepoConfig_FrameworkAllowSurvivesMerge: the allow-list is read
// by the indexer through IndexConfig, so it has to survive the
// global+workspace merge intact — a dropped entry would silently exclude
// a framework the repository asked to keep.
func TestGetRepoConfig_FrameworkAllowSurvivesMerge(t *testing.T) {
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)

	repoDir := t.TempDir()
	wsContent := `
index:
  frameworks:
    allow:
      - celery-dispatch
      - godot*
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("my-repo", repoDir)

	cfg := cm.GetRepoConfig("my-repo")
	require.NotNil(t, cfg)

	set := cfg.Index.AllowedFrameworks()
	assert.True(t, set.Allows("celery-dispatch"))
	assert.True(t, set.Allows("godot-autoload"), "trailing-* must survive as a prefix match")
	assert.False(t, set.Allows("django"))
	// Unrelated defaults are untouched by the new block.
	assert.Equal(t, runtime.NumCPU(), cfg.Index.Workers)
}

// TestGetRepoConfig_ExplicitWorkspaceValuesStillWin: seeding defaults
// must not override what the file actually sets.
func TestGetRepoConfig_ExplicitWorkspaceValuesStillWin(t *testing.T) {
	cm, err := NewConfigManager(filepath.Join(t.TempDir(), "config.yaml"))
	require.NoError(t, err)

	repoDir := t.TempDir()
	wsContent := `
index:
  workers: 3
  max_file_size: 2097152
`
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, ".gortex.yaml"), []byte(wsContent), 0644))
	cm.LoadWorkspaceConfig("my-repo", repoDir)

	cfg := cm.GetRepoConfig("my-repo")
	require.NotNil(t, cfg)
	assert.Equal(t, 3, cfg.Index.Workers)
	assert.Equal(t, int64(2097152), cfg.Index.MaxFileSize)
}
