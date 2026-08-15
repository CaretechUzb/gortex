package codex

// remove.go is the counterpart to Apply for `gortex uninstall --global`.
//
// # Ownership, not location
//
// Every deletion here is gated on evidence that Gortex authored the thing
// being deleted, never on the path it sits at. That distinction is the
// whole point of this file: `$HOME/.agents/skills` is a SHARED root —
// Codex, OpenCode and anything else honouring the vendor-neutral layout
// read it, and a user's own hand-written skills live there beside ours.
// Removing the directory would delete their work.
//
// The ownership predicate is the same one skills.go writes with: a
// `gortex-`-prefixed id from the shipped pack whose SKILL.md is still
// byte-identical to the body we render. A file that differs is a copy the
// user edited, and it is KEPT — the same posture as
// claudecode.SyncGlobalSkills, which refuses to delete a customised skill
// even when the active profile says it should not be installed. Losing a
// user's edits to a cleanup command is not a trade worth making.
//
// Merged config files are edited, never removed: the Gortex MCP stanza,
// the direct-namespace opt-in and the Gortex lifecycle hooks come out of
// ~/.codex/config.toml, and every other server, feature and hook in it
// survives. The rule block comes out of ~/.codex/AGENTS.md through the
// documented empty-body UpsertMarkedBlock path, which leaves the
// surrounding prose exactly where it was.

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

// RemoveGlobal strips the user-level Codex footprint `gortex install`
// wrote. Returns the number of artifacts removed-or-cleaned and any
// per-artifact failures — a partial clean still reports rather than
// aborting, because a cleanup that gives up halfway leaves the user worse
// off than one that says which two files it could not touch. Signature
// mirrors claudecode.Adapter.RemoveGlobal so `gortex uninstall` can call
// every host through the same shape.
func (a *Adapter) RemoveGlobal(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	if env.Home == "" {
		return 0, []string{"codex: global cleanup requires a resolved home directory"}
	}
	w := env.Stderr

	count := func(action agents.FileAction, err error, label string) {
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", label, err))
			return
		}
		if action.Action != agents.ActionSkip {
			removed++
		}
	}

	// 1. ~/.codex/config.toml — the MCP stanza, the direct-namespace
	// opt-in and the lifecycle hooks, in one merge so the file is
	// rewritten once rather than three times.
	configPath := filepath.Join(env.Home, ".codex", "config.toml")
	configAction, err := agents.MergeTOML(w, configPath, func(root map[string]any, _ bool) (bool, error) {
		changed := removeCodexMCPServer(root)
		if removeCodexDirectToolNamespaces(root) {
			changed = true
		}
		if removeCodexHooks(root) {
			changed = true
		}
		return changed, nil
	}, opts)
	count(configAction, err, configPath)

	// 2. ~/.codex/AGENTS.md — an empty body makes UpsertMarkedBlock drop
	// the marker-fenced block in place, which is the documented removal
	// path and the only one that preserves surrounding user prose.
	insPath := GlobalInstructionsPath(env.Home)
	insAction, err := agents.UpsertMarkedBlock(w, insPath, "",
		agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, opts)
	count(insAction, err, insPath)

	// 3. $HOME/.agents/skills/<id>/SKILL.md — the curated pack.
	skillsRemoved, skillFailures := removeCuratedSkills(env, opts)
	removed += skillsRemoved
	failures = append(failures, skillFailures...)

	return removed, failures
}

// GlobalArtifacts lists the user-level Codex paths that currently carry a
// Gortex footprint, sorted. It applies the SAME ownership tests
// RemoveGlobal does — a customised skill is absent from this list exactly
// because removal will keep it — so the uninstall wizard's preview can
// never promise a deletion that will not happen, nor stay silent about one
// that will.
func GlobalArtifacts(home string) []string {
	if home == "" {
		return nil
	}
	var present []string

	// The config file is listed when it carries any of the three things
	// RemoveGlobal takes out of it. The namespace opt-in is checked as a
	// string because Inspect does not report it, and `mcp__gortex` appears
	// in config.toml nowhere else.
	configPath := filepath.Join(home, ".codex", "config.toml")
	state := Inspect(home)
	if state.ConfigPresent &&
		(state.MCPServer || configuredHookCount(state.Hooks) > 0 || codexFileContains(configPath, codexGortexToolNamespace)) {
		present = append(present, configPath)
	}
	if ins := GlobalInstructionsPath(home); codexFileContains(ins, agents.GlobalRulesStartMarker) {
		present = append(present, ins)
	}
	if skills, err := Skills(); err == nil {
		root := codexSkillsRoot(home)
		for _, s := range skills {
			path := filepath.Join(root, s.ID, skillFileName)
			if isShippedCodexSkill(path, renderSkill(s)) {
				present = append(present, path)
			}
		}
	}

	sort.Strings(present)
	return present
}

// removeCodexMCPServer drops the gortex entry from Codex's TOML server
// table, pruning the table when it empties.
//
// agents.RemoveMCPServer is the shared helper for this, and it is not
// usable here: it keys on "mcpServers", the JSON spelling every other host
// uses, while Codex's TOML table is "mcp_servers". Same policy, different
// key — the user's other servers are never touched.
func removeCodexMCPServer(root map[string]any) bool {
	servers, ok := root["mcp_servers"].(map[string]any)
	if !ok {
		return false
	}
	if _, exists := servers["gortex"]; !exists {
		return false
	}
	delete(servers, "gortex")
	if len(servers) == 0 {
		delete(root, "mcp_servers")
	} else {
		root["mcp_servers"] = servers
	}
	return true
}

// removeCodexDirectToolNamespaces takes Gortex's two namespace spellings
// back out of features.code_mode.direct_only_tool_namespaces, pruning each
// level only when our removal is what emptied it. A namespace the user
// added for another server survives, and so does every other code_mode
// field.
func removeCodexDirectToolNamespaces(root map[string]any) bool {
	features, ok := root["features"].(map[string]any)
	if !ok {
		return false
	}
	codeMode, ok := features["code_mode"].(map[string]any)
	if !ok {
		return false
	}
	const field = "direct_only_tool_namespaces"
	namespaces, valid := codexStringList(codeMode[field])
	if !valid || len(namespaces) == 0 {
		return false
	}
	ours := map[string]bool{
		codexGortexToolNamespace:            true,
		codexGortexNonPrefixedToolNamespace: true,
	}
	kept := make([]string, 0, len(namespaces))
	for _, ns := range namespaces {
		if ours[ns] {
			continue
		}
		kept = append(kept, ns)
	}
	if len(kept) == len(namespaces) {
		return false
	}
	if len(kept) == 0 {
		delete(codeMode, field)
	} else {
		codeMode[field] = kept
	}
	if len(codeMode) == 0 {
		delete(features, "code_mode")
	}
	if len(features) == 0 {
		delete(root, "features")
	}
	return true
}

// removeCodexHooks drops every Gortex-authored hook entry, reusing the
// writer's own recognisers so a hook shape this package installs is always
// a hook shape it can find again. Entries authored by anyone else stay in
// their event's list, in order.
func removeCodexHooks(root map[string]any) bool {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false
	}
	recognisers := map[string]func(any) bool{
		"SessionStart":     codexHookEntryIsGortexSessionStart,
		"UserPromptSubmit": codexHookEntryIsGortexUserPromptSubmit,
		"PreToolUse":       codexHookEntryIsGortexPreToolUse,
		"PostToolUse":      codexHookEntryIsGortexPostToolUse,
	}
	changed := false
	for event, isGortex := range recognisers {
		entries, ok := codexHookList(hooks[event])
		if !ok {
			continue
		}
		kept := make([]any, 0, len(entries))
		for _, entry := range entries {
			if isGortex(entry) {
				changed = true
				continue
			}
			kept = append(kept, entry)
		}
		if len(kept) == len(entries) {
			continue
		}
		if len(kept) == 0 {
			delete(hooks, event)
		} else {
			hooks[event] = kept
		}
	}
	if !changed {
		return false
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	}
	return true
}

// removeCuratedSkills deletes the shipped pack from
// `$HOME/.agents/skills`, one SKILL.md at a time, and prunes each skill's
// directory only when nothing else is left in it. A user's own skill
// directory in the same root is never even looked at.
func removeCuratedSkills(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	skills, err := Skills()
	if err != nil {
		return 0, []string{fmt.Sprintf("codex skills: %v", err)}
	}
	root := codexSkillsRoot(env.Home)
	for _, s := range skills {
		dir := filepath.Join(root, s.ID)
		n, f := removeOwnedFile(env.Stderr, filepath.Join(dir, skillFileName), renderSkill(s), opts)
		removed += n
		failures = append(failures, f...)
		if n > 0 && !opts.DryRun {
			pruneEmptyDir(dir)
		}
	}
	if removed > 0 && !opts.DryRun {
		pruneEmptyDir(root)
		pruneEmptyDir(filepath.Dir(root))
	}
	return removed, failures
}

// removeOwnedFile deletes path only when its bytes are still exactly what
// Gortex shipped. Anything else is the user's edit and is kept with a
// warning, mirroring writeSkillFile's never-clobber posture on the way in.
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

// isShippedCodexSkill is the read-only half of removeOwnedFile, so the
// preview and the deletion cannot disagree about what is ours.
func isShippedCodexSkill(path, shipped string) bool {
	data, err := os.ReadFile(path)
	return err == nil && string(data) == shipped
}

// pruneEmptyDir removes dir only when it holds nothing. os.Remove refuses
// a non-empty directory, which is exactly the guard we want: a user file
// sitting next to a skill we deleted keeps its directory alive.
func pruneEmptyDir(dir string) {
	_ = os.Remove(dir)
}

func configuredHookCount(hooks map[string]int) int {
	total := 0
	for _, n := range hooks {
		total += n
	}
	return total
}

func codexFileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}
