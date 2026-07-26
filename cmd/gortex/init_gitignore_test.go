package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/excludes"
)

// TestInitRepoConfigLayersRepoGitignore pins #324. `gortex init` resolved its
// config without a repo path, so the repo's own .gitignore never reached
// indexer.New; the init walk then admitted gitignored trees (nested worktrees
// under an ignored directory, generated output) that the daemon's indexer
// prunes. Builtin-only exclusion is not enough — it covers node_modules/ and
// .next/ but not repo-specific ignores.
func TestInitRepoConfigLayersRepoGitignore(t *testing.T) {
	root := t.TempDir()
	writeInitConfigFile(t, filepath.Join(root, ".gitignore"), "tmp/\n/generated-out/\n")
	globalPath := filepath.Join(t.TempDir(), "config.yaml")
	writeInitConfigFile(t, globalPath, "")

	cfg := initRepoConfig(globalPath, root, nil)
	matcher := excludes.New(cfg.Index.Exclude)

	for _, dir := range []string{"tmp", "generated-out"} {
		if !matcher.MatchAbsDir(filepath.Join(root, dir), root, true) {
			t.Errorf("gitignored %q must be excluded from the init walk; Index.Exclude=%v",
				dir, cfg.Index.Exclude)
		}
	}
	// A tracked directory must still be walked, so the fix cannot be
	// "exclude everything".
	if matcher.MatchAbsDir(filepath.Join(root, "internal"), root, true) {
		t.Errorf("tracked directory must not be excluded; Index.Exclude=%v", cfg.Index.Exclude)
	}
}

// TestInitRepoConfigHonoursRespectGitignoreOptOut keeps the documented escape
// hatch working: a repo that opts out must still have its ignored trees walked.
func TestInitRepoConfigHonoursRespectGitignoreOptOut(t *testing.T) {
	root := t.TempDir()
	writeInitConfigFile(t, filepath.Join(root, ".gitignore"), "tmp/\n")
	writeInitConfigFile(t, filepath.Join(root, ".gortex.yaml"), "respect_gitignore: false\n")
	globalPath := filepath.Join(t.TempDir(), "config.yaml")
	writeInitConfigFile(t, globalPath, "")

	cfg := initRepoConfig(globalPath, root, nil)
	if excludes.New(cfg.Index.Exclude).MatchAbsDir(filepath.Join(root, "tmp"), root, true) {
		t.Errorf("respect_gitignore: false must not layer .gitignore; Index.Exclude=%v",
			cfg.Index.Exclude)
	}
}

func writeInitConfigFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
