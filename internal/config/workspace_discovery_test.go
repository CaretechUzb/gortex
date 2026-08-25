package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// writeAllowListConfig drops a .gortex.yaml carrying one recognisable
// setting, so a test can tell WHICH file was picked, not merely that one
// was.
func writeAllowListConfig(t *testing.T, dir, marker string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	path := filepath.Join(dir, WorkspaceConfigName)
	body := "index:\n  frameworks:\n    allow:\n      - " + marker + "\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	return path
}

// boundHomeAt makes the walk stop below `home` for the duration of a test.
// The real home directory is irrelevant under t.TempDir(), so without this
// the $HOME bound would never be exercised at all.
func boundHomeAt(t *testing.T, home string) {
	t.Helper()
	prev := userHomeDir
	userHomeDir = func() (string, error) { return home, nil }
	t.Cleanup(func() { userHomeDir = prev })
}

func TestFindWorkspaceConfig_AtRepoRoot(t *testing.T) {
	root := t.TempDir()
	want := writeAllowListConfig(t, root, "odoo")

	assert.Equal(t, want, FindWorkspaceConfig(root))
}

// The case this whole walk exists for: three checkouts tracked separately
// under one deployment that configures them once.
func TestFindWorkspaceConfig_FindsUmbrellaAboveCheckout(t *testing.T) {
	deploy := t.TempDir()
	want := writeAllowListConfig(t, deploy, "odoo")

	for _, checkout := range []string{"src/odoo", "src/addons", "src/local"} {
		repo := filepath.Join(deploy, checkout)
		require.NoError(t, os.MkdirAll(repo, 0o755))
		assert.Equal(t, want, FindWorkspaceConfig(repo), "checkout %s", checkout)
	}
}

// A worktree sits one level deeper than a plain checkout; the walk must
// still reach the umbrella rather than giving up at a fixed depth.
func TestFindWorkspaceConfig_ReachesThroughWorktreeNesting(t *testing.T) {
	deploy := t.TempDir()
	want := writeAllowListConfig(t, deploy, "odoo")

	repo := filepath.Join(deploy, "src", "local.worktrees", "aurora-redesign")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	assert.Equal(t, want, FindWorkspaceConfig(repo))
}

// Nearest wins — a checkout can always override its umbrella.
func TestFindWorkspaceConfig_RepoRootBeatsAncestor(t *testing.T) {
	deploy := t.TempDir()
	writeAllowListConfig(t, deploy, "umbrella")

	repo := filepath.Join(deploy, "src", "odoo")
	want := writeAllowListConfig(t, repo, "checkout")

	assert.Equal(t, want, FindWorkspaceConfig(repo))
}

func TestFindWorkspaceConfig_StopsBelowHome(t *testing.T) {
	home := t.TempDir()
	// A config sitting directly in $HOME must not be adopted: the global
	// layer already lives at ~/.gortex/config.yaml, and a stray file here
	// would silently reconfigure every repo under the home directory.
	writeAllowListConfig(t, home, "should-not-apply")
	boundHomeAt(t, home)

	repo := filepath.Join(home, "projects", "thing")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	assert.Empty(t, FindWorkspaceConfig(repo))
}

// The bound is "stop before $HOME", not "stop before anything under it":
// an umbrella one level below home still applies.
func TestFindWorkspaceConfig_AncestorBelowHomeStillApplies(t *testing.T) {
	home := t.TempDir()
	boundHomeAt(t, home)

	deploy := filepath.Join(home, "projects")
	want := writeAllowListConfig(t, deploy, "odoo")

	repo := filepath.Join(deploy, "src", "odoo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	assert.Equal(t, want, FindWorkspaceConfig(repo))
}

func TestFindWorkspaceConfig_NoConfigAnywhere(t *testing.T) {
	home := t.TempDir()
	boundHomeAt(t, home)

	repo := filepath.Join(home, "projects", "bare")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	assert.Empty(t, FindWorkspaceConfig(repo))
}

func TestFindWorkspaceConfig_EmptyPath(t *testing.T) {
	assert.Empty(t, FindWorkspaceConfig(""))
}

// A directory named .gortex.yaml is not a config file, and must not stop
// the walk at a false positive.
func TestFindWorkspaceConfig_IgnoresDirectoryNamedLikeConfig(t *testing.T) {
	deploy := t.TempDir()
	want := writeAllowListConfig(t, deploy, "odoo")

	repo := filepath.Join(deploy, "src", "odoo")
	require.NoError(t, os.MkdirAll(filepath.Join(repo, WorkspaceConfigName), 0o755))

	assert.Equal(t, want, FindWorkspaceConfig(repo))
}

// The end-to-end contract the daemon actually depends on: a repo with no
// config of its own still resolves the umbrella's settings.
func TestReadWorkspaceConfig_InheritsAncestorConfig(t *testing.T) {
	deploy := t.TempDir()
	writeAllowListConfig(t, deploy, "odoo")

	repo := filepath.Join(deploy, "src", "odoo")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	cm := &ConfigManager{
		workspace:      map[string]*Config{},
		workspacePaths: map[string]string{},
		logger:         zap.NewNop(),
		excludeCache:   newExcludeCache(),
	}

	cfg, authoritative := cm.readWorkspaceConfig("odoo", repo)
	require.True(t, authoritative)
	require.NotNil(t, cfg, "the umbrella config must be adopted by the nested checkout")
	assert.Equal(t, []string{"odoo"}, cfg.Index.Frameworks.Allow)
}

func TestReadWorkspaceConfig_NoConfigIsAuthoritativelyAbsent(t *testing.T) {
	home := t.TempDir()
	boundHomeAt(t, home)

	repo := filepath.Join(home, "projects", "bare")
	require.NoError(t, os.MkdirAll(repo, 0o755))

	cm := &ConfigManager{
		workspace:      map[string]*Config{},
		workspacePaths: map[string]string{},
		logger:         zap.NewNop(),
		excludeCache:   newExcludeCache(),
	}

	cfg, authoritative := cm.readWorkspaceConfig("bare", repo)
	assert.True(t, authoritative, "a genuinely absent config is an authoritative answer")
	assert.Nil(t, cfg)
}
