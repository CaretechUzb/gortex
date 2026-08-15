package opencode

// remove.go is the counterpart to applyGlobal for `gortex uninstall
// --global`.
//
// # Ownership, not location
//
// Every deletion is gated on evidence Gortex authored the thing being
// deleted, never on the path it sits at. `~/.config/opencode/skills` and
// `.../commands` are SHARED trees — a user's own skills and slash commands
// live there beside ours — so removing either directory would delete their
// work. The predicate is the one skills.go writes with: a shipped id whose
// file is still byte-identical to the body we render. A file that differs
// is KEPT with a warning, the same never-clobber posture writeCuratedFile
// takes on the way in.
//
// The bridge plugin is the one file Gortex owns end-to-end, and even it is
// checked: `plugin/gortex.js` is deleted only when it carries PluginMarker,
// so a same-named plugin somebody else wrote is left alone.
//
// # Stop-line: no MCP stanza and no instructions block at user scope
//
// applyGlobal writes neither, so RemoveGlobal removes neither. OpenCode's
// gortex MCP entry lives in the repo's own opencode.json (written by
// `gortex init`, removed by the repo-level half of `gortex uninstall`) and
// the community routing block lives in the repo's AGENTS.md. A `mcp.gortex`
// entry in `~/.config/opencode/opencode.json` was put there by the user,
// and a cleanup command that deletes config it never wrote is a cleanup
// command nobody runs twice.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/internalutil"
)

// RemoveGlobal strips the user-level OpenCode footprint `gortex install`
// wrote. Returns the number of artifacts removed-or-cleaned and any
// per-artifact failures — a partial clean still reports rather than
// aborting. Signature mirrors claudecode.Adapter.RemoveGlobal so `gortex
// uninstall` can call every host through the same shape.
func (a *Adapter) RemoveGlobal(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	if env.Home == "" {
		return 0, []string{"opencode: global cleanup requires a resolved home directory"}
	}

	// 1. ~/.config/opencode/plugin/gortex.js — the enforcement bridge.
	pluginRemoved, pluginFailures := removePlugin(env, opts)
	removed += pluginRemoved
	failures = append(failures, pluginFailures...)

	// 2. ~/.config/opencode/{skills,commands}/ — the curated packs.
	packRemoved, packFailures := removeCuratedPack(env, opts)
	removed += packRemoved
	failures = append(failures, packFailures...)

	return removed, failures
}

// GlobalArtifacts lists the user-level OpenCode paths that currently carry
// a Gortex footprint, sorted. It applies the SAME ownership tests
// RemoveGlobal does — a customised skill is absent from this list exactly
// because removal will keep it — so the uninstall wizard's preview can
// never promise a deletion that will not happen, nor stay silent about one
// that will.
func GlobalArtifacts(home string) []string {
	if home == "" {
		return nil
	}
	var present []string

	if path := PluginPath(home); opencodeFileContains(path, PluginMarker) {
		present = append(present, path)
	}
	for path, shipped := range ownedPackFiles(home) {
		if isShippedOpenCodeFile(path, shipped) {
			present = append(present, path)
		}
	}

	sort.Strings(present)
	return present
}

// removePlugin deletes the bridge, but only when the file on disk is
// actually ours. PluginMarker is the argv element that makes the file a
// Gortex bridge; matching on it rather than on the file name is what keeps
// a same-named plugin somebody else wrote out of the blast radius.
func removePlugin(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	path := PluginPath(env.Home)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	if !strings.Contains(string(data), PluginMarker) {
		internalutil.Warnf(env.Stderr, "keeping %s: it is not a Gortex bridge", path)
		return 0, nil
	}
	if opts.DryRun {
		return 1, nil
	}
	if err := os.Remove(path); err != nil {
		return 0, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	internalutil.Logf(env.Stderr, "[gortex uninstall] removed %s", path)
	pruneEmptyDir(filepath.Dir(path))
	return 1, nil
}

// ownedPackFiles maps every curated skill / command path to the body
// Gortex ships there. One map so the remover and the preview enumerate
// exactly the same set. A corpus that fails to parse yields nothing —
// the package's own tests turn that into a red build, and reporting an
// empty footprint beats guessing at one.
func ownedPackFiles(home string) map[string]string {
	out := make(map[string]string)
	if skills, err := Skills(); err == nil {
		root := globalSkillsDir(home)
		for _, s := range skills {
			out[filepath.Join(root, s.ID, skillFileName)] = renderSkill(s)
		}
	}
	if commands, err := Commands(); err == nil {
		root := globalCommandsDir(home)
		for _, c := range commands {
			out[filepath.Join(root, c.ID+commandFileExt)] = renderCommand(c)
		}
	}
	return out
}

// removeCuratedPack deletes the shipped skills and commands whose bytes
// are still ours, prunes each skill's own directory when it empties, and
// then the two roots. A user's skill directory in the same root is never
// looked at, and the roots survive as long as it does.
func removeCuratedPack(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	owned := ownedPackFiles(env.Home)
	paths := make([]string, 0, len(owned))
	for path := range owned {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	for _, path := range paths {
		n, f := removeOwnedFile(env.Stderr, path, owned[path], opts)
		removed += n
		failures = append(failures, f...)
		if n > 0 && !opts.DryRun && filepath.Base(path) == skillFileName {
			// Skills are <id>/SKILL.md; commands are flat <id>.md files.
			pruneEmptyDir(filepath.Dir(path))
		}
	}
	if removed > 0 && !opts.DryRun {
		pruneEmptyDir(globalSkillsDir(env.Home))
		pruneEmptyDir(globalCommandsDir(env.Home))
	}
	return removed, failures
}

// removeOwnedFile deletes path only when its bytes are still exactly what
// Gortex shipped. Anything else is the user's edit and is kept with a
// warning, mirroring writeCuratedFile's never-clobber posture on the way
// in.
func removeOwnedFile(w io.Writer, path, shipped string, opts agents.ApplyOpts) (removed int, failures []string) {
	existing, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	if string(existing) != shipped {
		internalutil.Warnf(w, "keeping customised %s", path)
		return 0, nil
	}
	if opts.DryRun {
		return 1, nil
	}
	if err := os.Remove(path); err != nil {
		return 0, []string{fmt.Sprintf("%s: %v", path, err)}
	}
	internalutil.Logf(w, "[gortex uninstall] removed %s", path)
	return 1, nil
}

func isShippedOpenCodeFile(path, shipped string) bool {
	data, err := os.ReadFile(path)
	return err == nil && string(data) == shipped
}

// pruneEmptyDir removes dir only when it holds nothing. os.Remove refuses
// a non-empty directory, which is exactly the guard we want: a user file
// sitting next to one we deleted keeps its directory alive.
func pruneEmptyDir(dir string) {
	_ = os.Remove(dir)
}

func opencodeFileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}
