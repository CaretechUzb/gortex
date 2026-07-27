package indexer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search"
)

// A repo's `.gortex.yaml` can change while the daemon runs, but an
// already-tracked repo short-circuits every path that would re-read it,
// and its per-repo Indexer froze the effective exclude list at
// construction — so a newly added exclude did not take effect until the
// process restarted (issue #320).
func TestMultiIndexer_RefreshRepoConfigs_AppliesEditedExcludeToLiveIndexer(t *testing.T) {
	dir := setupRepoDir(t, "myrepo")
	writeFile(t, filepath.Join(dir, "drop.go"), `package main

func Dropped() {}
`)

	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{Repos: []config.RepoEntry{{Path: dir, Name: "myrepo"}}}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())

	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)

	g := graph.New()
	mi := NewMultiIndexer(g, newTestRegistry(), search.NewBM25(), cm, zap.NewNop())
	_, err = mi.IndexAll()
	require.NoError(t, err)
	require.NotEmpty(t, g.FindNodesByName("Dropped"), "precondition: drop.go is indexed")

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"),
		[]byte("exclude:\n  - \"drop.go\"\n"), 0o644))

	assert.Equal(t, 1, mi.RefreshRepoConfigs())
	assert.Contains(t, cm.EffectiveExclude("myrepo"), "drop.go",
		"the refreshed config must reach the ConfigManager")

	res, err := mi.IncrementalReindexRepo("myrepo", nil)
	require.NoError(t, err)
	require.NotNil(t, res)

	assert.Empty(t, g.FindNodesByName("Dropped"),
		"a file excluded after tracking must leave the graph on the next pass")
	assert.NotEmpty(t, g.FindNodesByName("Hello"),
		"unrelated symbols must survive the refresh")
}

func TestMultiIndexer_RefreshRepoConfigs_NoConfigManager(t *testing.T) {
	mi := NewMultiIndexer(graph.New(), newTestRegistry(), search.NewBM25(), nil, zap.NewNop())
	assert.Equal(t, 0, mi.RefreshRepoConfigs())
}
