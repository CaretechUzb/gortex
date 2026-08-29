package mcp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// allowListRepo writes a repo directory whose .gortex.yaml narrows
// index.frameworks.allow to one framework.
func allowListRepo(t *testing.T, framework string) string {
	t.Helper()
	dir := t.TempDir()
	body := "index:\n  frameworks:\n    allow:\n      - " + framework + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte(body), 0o644))
	return dir
}

func newAllowListConfigManager(t *testing.T) *config.ConfigManager {
	t.Helper()
	tmpCfg := filepath.Join(t.TempDir(), "config.yaml")
	gc := &config.GlobalConfig{}
	gc.SetConfigPath(tmpCfg)
	require.NoError(t, gc.Save())
	cm, err := config.NewConfigManager(tmpCfg)
	require.NoError(t, err)
	return cm
}

// The config manager's workspace map only ever grows — it caches a prefix
// the first time a config is published for it and has no removal path — so
// reading it here kept an untracked repo voting in the allow-list union
// and listed it in allowed_in, long after it left the graph.
func TestFrameworkAllowListsByRepo_SkipsUntrackedRepos(t *testing.T) {
	cm := newAllowListConfigManager(t)
	cm.LoadWorkspaceConfig("tracked", allowListRepo(t, "odoo"))
	cm.LoadWorkspaceConfig("gone", allowListRepo(t, "django"))
	require.ElementsMatch(t, []string{"gone", "tracked"}, cm.WorkspacePrefixes(),
		"both prefixes must be cached, or the test proves nothing")

	// Only "tracked" has nodes, which is what RepoPrefixes reports.
	g := graph.New()
	g.AddNode(&graph.Node{
		ID:         "tracked/main.go",
		Kind:       graph.KindFile,
		Name:       "main.go",
		FilePath:   "tracked/main.go",
		RepoPrefix: "tracked",
	})

	s := &Server{graph: g, configManager: cm}

	perRepo := s.frameworkAllowListsByRepo()
	assert.Contains(t, perRepo, "tracked")
	assert.NotContains(t, perRepo, "gone", "an untracked repo must not govern the workspace")

	// The user-visible symptom: django was allowed only by the repo that
	// left, so nothing should admit it any more.
	rows := s.frameworkInventory()
	assert.True(t, inventoryRow(t, rows, "odoo").Active)
	assert.False(t, inventoryRow(t, rows, "django").Active,
		"a framework allowed only by an untracked repo must read as excluded")
}

// With nothing tracked there is no indexer view to prefer, and the config
// manager's is still the right answer for discovery — this analyzer exists
// to say what a configuration would allow.
func TestFrameworkAllowListsByRepo_FallsBackWhenNothingTracked(t *testing.T) {
	cm := newAllowListConfigManager(t)
	cm.LoadWorkspaceConfig("repo-a", allowListRepo(t, "odoo"))

	s := &Server{configManager: cm}

	perRepo := s.frameworkAllowListsByRepo()
	require.Contains(t, perRepo, "repo-a")
	assert.True(t, perRepo["repo-a"].Allows("odoo"))
	assert.False(t, perRepo["repo-a"].Allows("django"))
}
