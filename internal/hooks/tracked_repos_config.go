package hooks

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
)

// The tracked-repo registry is `~/.gortex/config.yaml` — a daemon status row
// for a repo the daemon holds no index for is itself synthesised from that
// file, so the config is the membership source of truth and the status call
// only adds live counters on top.
//
// Reading it directly is what the hot path wants. Asking the daemon instead
// cost a full ControlStatus round trip per hook fire — every tracked repo
// marshalled with its file/node/edge counts and memory breakdown — measured at
// ~76 ms warm, over a second under load, and often enough hitting its 2 s
// deadline that the hook then answered from an empty list. An empty list reads
// as "no repo owns this path" downstream, so a timeout did not merely make the
// hook slow: it silently downgraded an indexed-file decision to generic
// guidance.
//
// Membership is all the hot path needs — repoRootForFile and the scope gate
// read Path, hookRepoScope reads Path and Prefix. The live counters stay zero
// here; SessionStart is the only consumer that reads them and it keeps its own
// status call, firing once per session rather than once per tool call.

var (
	configReposOnce sync.Once
	configReposVal  []daemon.TrackedRepoStatus
	configReposOK   bool
)

// configTrackedRepos returns the tracked-repo registry read from the user's
// global config, falling back to the daemon only when that file is absent or
// unparseable. The result is memoised for the process: a hook invocation is
// short-lived and the registry cannot change underneath it.
func configTrackedRepos() []daemon.TrackedRepoStatus {
	configReposOnce.Do(func() {
		configReposVal, configReposOK = loadConfigTrackedRepos()
	})
	if configReposOK {
		return configReposVal
	}
	// Unreadable config: fall back to the daemon rather than report an empty
	// registry, which downstream cannot distinguish from "nothing is tracked".
	return cachedTrackedRepos()
}

// loadConfigTrackedRepos flattens the global config's repo entries — both the
// top-level list and every named project's — into status rows. ok is false
// only when the registry could not be established at all (missing or invalid
// config file); a config that parses but declares no repos is authoritative
// and returns (nil, true), because that genuinely means nothing is tracked.
func loadConfigTrackedRepos() ([]daemon.TrackedRepoStatus, bool) {
	return loadTrackedReposFromPath(config.DefaultGlobalConfigPath())
}

// loadTrackedReposFromPath is loadConfigTrackedRepos with an explicit config
// path, so tests exercise the registry without a real ~/.gortex/config.yaml.
func loadTrackedReposFromPath(path string) ([]daemon.TrackedRepoStatus, bool) {
	if path == "" {
		return nil, false
	}
	if _, err := os.Stat(path); err != nil {
		return nil, false // absent → let the daemon answer
	}
	gc, err := config.LoadGlobal(path)
	if err != nil || gc == nil {
		return nil, false
	}

	var out []daemon.TrackedRepoStatus
	seen := make(map[string]bool)
	add := func(e config.RepoEntry) {
		p := strings.TrimSpace(e.Path)
		if p == "" {
			return
		}
		// Deliberately symlink-naive, matching repoRootForFile and
		// hookRepoScope: a symlinked worktree resolves to no repo and
		// degrades to the labeled fallback rather than a guessed attribution.
		if !filepath.IsAbs(p) {
			abs, absErr := filepath.Abs(p)
			if absErr != nil {
				return
			}
			p = abs
		}
		p = filepath.Clean(p)
		if seen[p] {
			return
		}
		seen[p] = true
		out = append(out, daemon.TrackedRepoStatus{
			Path:      p,
			Prefix:    config.ResolvePrefix(e),
			Name:      e.Name,
			Ref:       e.Ref,
			Workspace: e.Workspace,
			Project:   e.Project,
		})
	}

	for _, e := range gc.Repos {
		add(e)
	}
	// Project-nested entries are tracked the same as top-level ones; a
	// registry that dropped them would report a tracked path as untracked,
	// which is the exact failure this file exists to remove.
	for _, pc := range gc.Projects {
		for _, e := range pc.Repos {
			add(e)
		}
	}
	return out, true
}
