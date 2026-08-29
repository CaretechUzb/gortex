package config

import (
	"os"
	"path/filepath"
)

// WorkspaceConfigName is the per-repo config file every layer looks for.
const WorkspaceConfigName = ".gortex.yaml"

// userHomeDir is indirected so a test can bound the walk somewhere other
// than the real home directory.
var userHomeDir = os.UserHomeDir

// FindWorkspaceConfig returns the nearest `.gortex.yaml` at or above
// repoPath, or "" when none applies.
//
// A tracked repo is often a checkout nested inside a larger one. An Odoo
// deployment tracks `<deploy>/src/odoo`, `<deploy>/src/addons` and
// `<deploy>/src/local` as three separate repos, but the settings that
// describe the deployment — which frameworks to run, what to exclude —
// describe all three at once. They belong in one file at `<deploy>/`,
// not copied into every checkout and re-copied whenever one is added.
//
// Reading only repoPath made such a file silently inert while the CLI's
// own lookup walked up and reported it as the active workspace config.
// The two layers disagreed, so a file the user could see listed had no
// effect on indexing. Sharing this walk is what keeps them honest.
//
// A `.gortex.yaml` AT repoPath still wins: the walk starts there and
// returns the first hit, so a checkout can always override its umbrella.
//
// The walk stops before $HOME and before the filesystem root.
// `~/.gortex/config.yaml` is already the global layer, and a stray
// `.gortex.yaml` in a home or root directory silently reconfiguring every
// repo beneath it is a surprise rather than a feature.
func FindWorkspaceConfig(repoPath string) string {
	if repoPath == "" {
		return ""
	}
	dir, err := filepath.Abs(repoPath)
	if err != nil {
		dir = repoPath
	}
	// A home directory that cannot be determined simply does not bound the
	// walk; the filesystem-root guard below still terminates it.
	home, _ := userHomeDir()

	for {
		candidate := filepath.Join(dir, WorkspaceConfigName)
		if st, statErr := os.Stat(candidate); statErr == nil && !st.IsDir() {
			return candidate
		}
		parent := filepath.Dir(dir)
		switch {
		case parent == dir:
			// dir is already the filesystem root, and carried no config.
			return ""
		case filepath.Dir(parent) == parent:
			// The next step up would be the filesystem root itself, which
			// is never a legitimate home for a workspace config.
			return ""
		case home != "" && parent == home:
			return ""
		}
		dir = parent
	}
}
