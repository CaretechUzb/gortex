package copilotcli

// remove.go is the counterpart to Apply for `gortex uninstall --global`.
//
// # Ownership, not location
//
// Every deletion is gated on evidence Gortex authored the thing being
// deleted, never on the path it sits at. ~/.copilot/skills and
// ~/.copilot/agents hold the user's own skills and custom agents beside
// ours; deleting either directory would destroy their work. The predicate
// is the one skills.go / subagents.go write with — a shipped id whose file
// is still byte-identical to the body we render — and a file that differs
// is KEPT with a warning, the same never-clobber posture Apply takes on
// the way in.
//
// ~/.copilot/hooks/gortex.json is ours by name, but it is still edited
// rather than blindly deleted: the CLI merges every *.json in that
// directory, so a user who appended their own entry to our file would lose
// it. Our entries come out through the writer's own recogniser, and the
// file is deleted only once nothing but our scaffolding is left.
//
// The MCP stanza and the rule block go through the shared removal paths —
// agents.RemoveMCPServer and an empty-body agents.UpsertMarkedBlock — so
// other servers and the user's surrounding instructions prose survive.

import (
	"encoding/json"
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

// RemoveGlobal strips the user-level Copilot CLI footprint `gortex
// install` wrote. Returns the number of artifacts removed-or-cleaned and
// any per-artifact failures — a partial clean still reports rather than
// aborting. Signature mirrors claudecode.Adapter.RemoveGlobal so `gortex
// uninstall` can call every host through the same shape.
func (a *Adapter) RemoveGlobal(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	if copilotConfigHome(env) == "" {
		return 0, []string{"copilot-cli: global cleanup requires a resolved home directory"}
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

	// 1. ~/.copilot/mcp-config.json — drop only the "gortex" server.
	mcpPath := MCPConfigPath(env)
	mcpAction, err := agents.MergeJSON(w, mcpPath, func(root map[string]any, _ bool) (bool, error) {
		return agents.RemoveMCPServer(root, serverName), nil
	}, opts)
	count(mcpAction, err, mcpPath)

	// 2. ~/.copilot/copilot-instructions.md — an empty body makes
	// UpsertMarkedBlock drop the marker-fenced block in place, which is
	// the documented removal path and the only one that preserves the
	// user's surrounding prose.
	insPath := GlobalInstructionsPath(env)
	insAction, err := agents.UpsertMarkedBlock(w, insPath, "",
		agents.GlobalRulesStartMarker, agents.GlobalRulesEndMarker, opts)
	count(insAction, err, insPath)

	// 3. ~/.copilot/hooks/gortex.json — our entries out, the user's kept.
	hookAction, err := removeCopilotHooks(env, opts)
	count(hookAction, err, HookConfigPath(env))

	// 4. ~/.copilot/skills/<id>/SKILL.md and ~/.copilot/agents/<id>.agent.md.
	ownedRemoved, ownedFailures := removeCopilotOwnedFiles(env, opts)
	removed += ownedRemoved
	failures = append(failures, ownedFailures...)

	return removed, failures
}

// GlobalArtifacts lists the user-level Copilot CLI paths that currently
// carry a Gortex footprint, sorted. It applies the SAME ownership tests
// RemoveGlobal does — a customised skill is absent from this list exactly
// because removal will keep it — so the uninstall wizard's preview can
// never promise a deletion that will not happen, nor stay silent about one
// that will.
func GlobalArtifacts(home string) []string {
	env := agents.Env{Home: home}
	if copilotConfigHome(env) == "" {
		return nil
	}
	var present []string

	// Parsed rather than grepped: a preview that fires on the word "gortex"
	// anywhere in the file would promise to edit a config removal will not
	// touch.
	if mcpConfigHasGortexServer(MCPConfigPath(env)) {
		present = append(present, MCPConfigPath(env))
	}
	if copilotFileContains(GlobalInstructionsPath(env), agents.GlobalRulesStartMarker) {
		present = append(present, GlobalInstructionsPath(env))
	}
	if hookConfigHasGortexEntry(HookConfigPath(env)) {
		present = append(present, HookConfigPath(env))
	}
	for path, shipped := range copilotOwnedFiles(env) {
		if isShippedCopilotFile(path, shipped) {
			present = append(present, path)
		}
	}

	sort.Strings(present)
	return present
}

// removeCopilotHooks takes our entries out of the hook config and deletes
// the file only when nothing else was in it.
//
// The file carries our name, which is tempting to read as "ours to
// delete". It is not sufficient: the CLI merges every hooks/*.json, so a
// user who added an entry to the file we created still owns that entry.
// Stripping first and deleting only on an empty remainder is what keeps
// both facts true.
func removeCopilotHooks(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, error) {
	path := HookConfigPath(env)
	action := agents.FileAction{Path: path, Action: agents.ActionSkip}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		action.Reason = "absent"
		return action, nil
	}
	if err != nil {
		return agents.FileAction{}, fmt.Errorf("read %s: %w", path, err)
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		// A shape we cannot parse is a shape we must not delete.
		action.Reason = "unparsable"
		return action, nil
	}
	if !stripGortexCopilotHooks(root) {
		action.Reason = "no-gortex-hooks"
		return action, nil
	}

	hooks, _ := root["hooks"].(map[string]any)
	if len(hooks) == 0 && onlyGortexScaffolding(root) {
		// Nothing but the version / disableAllHooks pair upsertCopilotHooks
		// seeds is left, so the whole file is ours.
		if opts.DryRun {
			return agents.FileAction{Path: path, Action: agents.ActionWouldDelete}, nil
		}
		if err := os.Remove(path); err != nil {
			return agents.FileAction{}, fmt.Errorf("remove %s: %w", path, err)
		}
		internalutil.Logf(env.Stderr, "[gortex uninstall] removed %s", path)
		pruneEmptyDir(filepath.Dir(path))
		return agents.FileAction{Path: path, Action: agents.ActionDelete}, nil
	}

	if opts.DryRun {
		return agents.FileAction{Path: path, Action: agents.ActionWouldMerge, Keys: []string{"hooks"}}, nil
	}
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return agents.FileAction{}, fmt.Errorf("marshal %s: %w", path, err)
	}
	if err := agents.AtomicWriteFile(path, out, 0o644); err != nil {
		return agents.FileAction{}, err
	}
	internalutil.Logf(env.Stderr, "[gortex uninstall] removed the gortex hook entries from %s", path)
	return agents.FileAction{Path: path, Action: agents.ActionMerge, Keys: []string{"hooks"}}, nil
}

// onlyGortexScaffolding reports whether nothing is left in the parsed
// config beyond the keys upsertCopilotHooks seeds. A top-level key we did
// not write is the user's, and its presence is what turns "delete the
// file" back into "rewrite it" — the file name alone is never enough
// evidence to destroy content.
func onlyGortexScaffolding(root map[string]any) bool {
	ours := map[string]bool{"version": true, "disableAllHooks": true, "hooks": true}
	for key := range root {
		if !ours[key] {
			return false
		}
	}
	return true
}

// stripGortexCopilotHooks removes every Gortex-authored entry from a
// parsed hook config, reusing the writer's own recogniser so an entry this
// package installs is always an entry it can find again. Reports whether
// anything came out.
func stripGortexCopilotHooks(root map[string]any) bool {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false
	}
	changed := false
	for _, event := range copilotHookEventNames {
		entries, ok := copilotHookList(hooks[event])
		if !ok {
			continue
		}
		kept := make([]any, 0, len(entries))
		for _, entry := range entries {
			if isGortexCopilotHookEntry(entry) {
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

// mcpConfigHasGortexServer is the read-only half of
// agents.RemoveMCPServer's precondition, so the wizard preview and the
// deletion cannot disagree about whether the registry carries our entry.
func mcpConfigHasGortexServer(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	_, ok := root.MCPServers[serverName]
	return ok
}

// hookConfigHasGortexEntry is the read-only half of
// stripGortexCopilotHooks, so the wizard preview and the deletion cannot
// disagree about whether the hook file carries anything of ours.
func hookConfigHasGortexEntry(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return false
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return false
	}
	for _, event := range copilotHookEventNames {
		entries, ok := copilotHookList(hooks[event])
		if !ok {
			continue
		}
		for _, entry := range entries {
			if isGortexCopilotHookEntry(entry) {
				return true
			}
		}
	}
	return false
}

// copilotOwnedFiles maps every curated skill / sub-agent path to the body
// Gortex ships there. One map so the remover and the preview enumerate
// exactly the same set.
func copilotOwnedFiles(env agents.Env) map[string]string {
	out := make(map[string]string)
	skills := CuratedSkills()
	for _, id := range CuratedSkillNames() {
		body, ok := skills[id]
		if !ok {
			continue
		}
		if path := curatedSkillPath(env, id); path != "" {
			out[path] = body
		}
	}
	defs := SubAgents()
	for _, id := range SubAgentNames() {
		body, ok := defs[id]
		if !ok {
			continue
		}
		if path := subAgentPath(env, id); path != "" {
			out[path] = body
		}
	}
	return out
}

// removeCopilotOwnedFiles deletes the curated skills and sub-agents whose
// bytes are still ours, prunes each skill's own directory when it empties,
// and then the two roots. A user's skill directory in the same root is
// never looked at, and the roots survive as long as it does.
func removeCopilotOwnedFiles(env agents.Env, opts agents.ApplyOpts) (removed int, failures []string) {
	owned := copilotOwnedFiles(env)
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
			// Skills are <id>/SKILL.md; sub-agents are flat files.
			pruneEmptyDir(filepath.Dir(path))
		}
	}
	if removed > 0 && !opts.DryRun {
		home := copilotConfigHome(env)
		pruneEmptyDir(filepath.Join(home, skillsDirName))
		pruneEmptyDir(filepath.Join(home, subAgentsDirName))
	}
	return removed, failures
}

// removeOwnedFile deletes path only when its bytes are still exactly what
// Gortex shipped. Anything else is the user's edit and is kept with a
// warning, mirroring the never-clobber WriteIfNotExists posture on the way
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

func isShippedCopilotFile(path, shipped string) bool {
	data, err := os.ReadFile(path)
	return err == nil && string(data) == shipped
}

// pruneEmptyDir removes dir only when it holds nothing. os.Remove refuses
// a non-empty directory, which is exactly the guard we want: a user file
// sitting next to one we deleted keeps its directory alive.
func pruneEmptyDir(dir string) {
	_ = os.Remove(dir)
}

func copilotFileContains(path, needle string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}
