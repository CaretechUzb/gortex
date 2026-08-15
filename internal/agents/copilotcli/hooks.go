package copilotcli

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/agents"
)

// hooks.go writes ~/.copilot/hooks/gortex.json — the Copilot CLI's
// lifecycle hook config.
//
// # Why user-level only
//
// The CLI reads hooks from ~/.copilot/hooks/*.json AND .github/hooks/*.json.
// Only the first is written. A repo-level hook file is committed, so it
// would fire `gortex hook` on every teammate's machine the moment they
// clone — including the ones with no gortex installed, who would get a
// failed subprocess on every tool call. Hooks are a machine-local
// posture; they are installed by `gortex install`, never by `gortex init`.
//
// # Why four events and not fourteen
//
// The CLI defines fourteen lifecycle events. Gortex produces a payload
// for four of them; the other ten (userPromptTransformed, preCompact,
// agentStop, subagentStart, subagentStop, errorOccurred,
// permissionRequest, notification, postToolUseFailure, sessionEnd) would
// map to nothing. Registering an event is not free — it spawns a
// subprocess per occurrence — so an event that returns nothing is a pure
// per-turn tax.
//
// # Why every entry carries two command fields
//
// A hook entry has SEPARATE `bash` and `powershell` fields; the CLI picks
// by platform rather than shelling one string through whatever shell is
// present. A POSIX-only entry therefore leaves Windows users with a hook
// that silently never runs, which reads exactly like "Gortex configured,
// Gortex ignored". Both fields are always emitted.
//
// # Matchers
//
// Matchers apply to preToolUse / postToolUse / permissionRequest, are
// matched against the tool name, and the CLI anchors them as
// `^(?:PATTERN)$`. The built-in tool names are LOWERCASE — `view`, not
// `View` — and `powershell` is a first-class sibling of `bash`, so a
// matcher that lists only `bash` leaves the entire Windows shell surface
// unguarded.
//
// Docs: https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/create-hooks

const (
	// hooksDirName holds one JSON file per hook source.
	hooksDirName = "hooks"
	// hookFileName is ours; a user's own hook files sit beside it.
	hookFileName = "gortex.json"
	// hookConfigVersion is the schema version the CLI expects.
	hookConfigVersion = 1
	// hookTimeoutSeconds bounds each spawn. Timeouts fail OPEN, so this
	// is a latency ceiling rather than a safety control: the graph probe
	// the handler runs is local and sub-second, and a hung daemon must
	// not stall every tool call for longer than a user would tolerate.
	hookTimeoutSeconds = 10
	// hookAgentFlag routes the payload to the Copilot decoder. It must
	// match the --agent value cmd/gortex dispatches on.
	hookAgentFlag = "copilot-cli"
)

// copilotHookEventNames translates Gortex's PascalCase event vocabulary
// into the camelCase names the CLI reads on disk.
//
// The split is deliberate and load-bearing in one direction only:
// everything Gortex measures — internal/hooks telemetry, `gortex doctor`
// — keys on the PascalCase spellings, so those are what Inspect reports
// and what HookEvents lists. The camelCase spellings exist nowhere but
// this map, the writer, and the recogniser. Leaking them outward makes
// every event score zero runs in doctor's tally and turns a healthy
// install into a permanent "configured but none has run" blocker.
//
// Note userPromptSubmitted: the CLI's name is past tense, Claude's
// (UserPromptSubmit) is not.
var copilotHookEventNames = map[string]string{
	"SessionStart":     "sessionStart",
	"UserPromptSubmit": "userPromptSubmitted",
	"PreToolUse":       "preToolUse",
	"PostToolUse":      "postToolUse",
}

// copilotToolMatcherNames are the CLI's built-in tools whose use Gortex
// has something to say about: reads (view), searches (grep, rg, glob),
// shells (bash, powershell) and writes (create, edit). Sorted so the
// rendered alternation is byte-stable.
//
// The CLI's remaining built-ins — ask_user, task, web_fetch, web_search,
// update_todo — never touch indexed source, so matching them would spawn
// a subprocess for nothing.
var copilotToolMatcherNames = []string{
	"bash", "create", "edit", "glob", "grep", "powershell", "rg", "view",
}

// copilotToolMatcher renders the anchored alternation. Anchoring is
// written explicitly rather than relying on the CLI to wrap it, so the
// pattern means the same thing if it is ever read by anything else.
func copilotToolMatcher() string {
	return "^(?:" + strings.Join(copilotToolMatcherNames, "|") + ")$"
}

// HookConfigPath is ~/.copilot/hooks/gortex.json for this Env.
func HookConfigPath(env agents.Env) string {
	home := copilotConfigHome(env)
	if home == "" {
		return ""
	}
	return filepath.Join(home, hooksDirName, hookFileName)
}

// hookBinary resolves the gortex binary the entries invoke.
//
// Env.HookCommand is "<binary> hook" — the caller resolves the binary
// once (through agents.ResolveGortexHookBinary, which refuses a `go run`
// temp build) and appends the subcommand. That subcommand is stripped
// back off here because the PowerShell field has to quote the path on its
// own, which is impossible once binary and arguments are one string.
func hookBinary(env agents.Env) string {
	if base := strings.TrimSpace(env.HookCommand); base != "" {
		if bin, ok := strings.CutSuffix(base, " hook"); ok {
			return strings.TrimSpace(bin)
		}
		return base
	}
	return agents.ResolveGortexHookBinary()
}

// hookArgs is the tail both shells append to the binary.
const hookArgs = " hook --agent=" + hookAgentFlag

// bashHookCommand renders the POSIX form. Backslashes are normalised to
// forward slashes for the same reason the Claude adapter does it: a
// native Windows path inside an unquoted shell word is mangled by Git
// Bash, and no real POSIX binary path uses a backslash as a separator.
func bashHookCommand(env agents.Env) string {
	return posixQuote(strings.ReplaceAll(hookBinary(env), `\`, "/")) + hookArgs
}

// powershellHookCommand renders the Windows form.
//
// A bare path runs as a command; a quoted one is just a string
// expression PowerShell echoes, so quoting forces the call operator `&`
// in front of it. Quoting is therefore applied only when the path needs
// it — which also keeps `&` out of the JSON in the common case, where
// encoding/json would escape it to the unreadable "&".
func powershellHookCommand(env agents.Env) string {
	bin := hookBinary(env)
	if powershellBareSafe(bin) {
		return bin + hookArgs
	}
	return "& " + powershellQuote(bin) + hookArgs
}

// powershellBareSafe reports whether a path can be typed as a bare
// PowerShell command. Backslashes are fine — PowerShell's escape
// character is the backtick — so a native Windows path usually qualifies.
func powershellBareSafe(s string) bool {
	return s != "" && !strings.ContainsAny(s, " \t\n\"'`$;|&(){}[]<>@#,")
}

// posixQuote single-quotes s when it carries anything a POSIX shell would
// interpret. A plain path is left bare so the written config stays
// readable.
func posixQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$`&;|<>()*?[]{}#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// powershellQuote always single-quotes: a Windows path routinely contains
// spaces ("C:\Program Files\..."), and inside PowerShell single quotes
// the only escape needed is a doubled quote.
func powershellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// desiredHookEntry builds the entry for one PascalCase event.
//
// A matcher is emitted only for the tool-scoped events. sessionStart and
// userPromptSubmitted have no tool name to match against, and a matcher
// on them is at best ignored.
func desiredHookEntry(event string, env agents.Env) map[string]any {
	entry := map[string]any{
		"type":       "command",
		"bash":       bashHookCommand(env),
		"powershell": powershellHookCommand(env),
		// Relative cwd: the hook must run inside the session's repository
		// so the handler resolves the same workspace the tools serve.
		"cwd":        ".",
		"timeoutSec": hookTimeoutSeconds,
	}
	if event == "PreToolUse" || event == "PostToolUse" {
		entry["matcher"] = copilotToolMatcher()
	}
	return entry
}

// upsertCopilotHooks merges our four events into a parsed hooks config.
// Returns whether anything changed.
func upsertCopilotHooks(root map[string]any, env agents.Env, opts agents.ApplyOpts) bool {
	changed := false

	// Only ever seeded, never corrected: `version` is the CLI's to bump
	// and `disableAllHooks: true` is a user kill-switch we must not flip
	// back on their behalf.
	if _, exists := root["version"]; !exists {
		root["version"] = hookConfigVersion
		changed = true
	}
	if _, exists := root["disableAllHooks"]; !exists {
		root["disableAllHooks"] = false
		changed = true
	}

	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		if _, exists := root["hooks"]; exists {
			// Present but not an object: a shape we don't understand and
			// must not overwrite.
			return changed
		}
		hooks = make(map[string]any)
	}

	for _, event := range HookEvents {
		if upsertCopilotHookEvent(hooks, copilotHookEventNames[event], desiredHookEntry(event, env), opts) {
			changed = true
		}
	}
	if changed {
		root["hooks"] = hooks
	}
	return changed
}

// upsertCopilotHookEvent reconciles one event's entry list against the
// single entry we want there. The policy, modelled on the Codex adapter:
//
//   - entries we did not author are kept byte-identical, always;
//   - our entry is replaced in place, so N re-runs leave exactly one;
//   - a Gortex entry whose matcher the user NARROWED is left alone —
//     they deliberately scoped our hook, and silently re-widening it on
//     the next `gortex install` is the kind of surprise that gets hooks
//     turned off wholesale. opts.Force is the way back to our matcher.
func upsertCopilotHookEvent(hooks map[string]any, event string, want map[string]any, opts agents.ApplyOpts) bool {
	entries, ok := copilotHookList(hooks[event])
	if !ok {
		// Present but not a list: not ours to reshape.
		return false
	}

	found := false
	changed := false
	kept := make([]any, 0, len(entries)+1)
	for _, entry := range entries {
		if !isGortexCopilotHookEntry(entry) {
			kept = append(kept, entry)
			continue
		}
		if opts.Force {
			changed = true
			continue
		}
		if !found && (copilotHookMatcherIsNarrowed(entry, want) || agents.MCPEntriesEqual(entry, want)) {
			found = true
			kept = append(kept, entry)
			continue
		}
		// A Gortex entry that is neither current nor a deliberate
		// narrowing: a stale command, timeout, or a duplicate from an
		// older run. Drop it instead of accumulating.
		changed = true
	}
	if !found {
		kept = append(kept, want)
		changed = true
	}
	if !changed {
		return false
	}
	hooks[event] = kept
	return true
}

// copilotHookList normalises an event's value to a slice. A missing key
// is an empty list (ok); a non-list is a shape we refuse to touch.
func copilotHookList(v any) ([]any, bool) {
	if v == nil {
		return nil, true
	}
	switch list := v.(type) {
	case []any:
		return append([]any(nil), list...), true
	case []map[string]any:
		out := make([]any, 0, len(list))
		for _, entry := range list {
			out = append(out, entry)
		}
		return out, true
	default:
		return nil, false
	}
}

// isGortexCopilotHookEntry reports whether an entry invokes our hook,
// in either shell field. Recognition is by command content rather than a
// marker key, so an entry written by an older gortex — or by a user who
// copied ours and edited the matcher — is still recognised as ours.
func isGortexCopilotHookEntry(entry any) bool {
	fields, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	for _, key := range []string{"bash", "powershell", "command"} {
		if cmd, _ := fields[key].(string); commandInvokesCopilotHook(cmd) {
			return true
		}
	}
	return false
}

func commandInvokesCopilotHook(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return false
	}
	lower := strings.ToLower(cmd)
	if !strings.Contains(lower, "gortex") || !strings.Contains(lower, "hook") {
		return false
	}
	return strings.Contains(cmd, "--agent="+hookAgentFlag) || strings.Contains(cmd, "--agent "+hookAgentFlag)
}

// copilotHookMatcherIsNarrowed reports whether entry's matcher is a
// strict subset of the one we want — the fingerprint of a user who
// trimmed our alternation down to the tools they care about.
//
// Only the anchored alternation form is understood. A matcher rewritten
// into any other shape is not recognised as a narrowing, so it falls
// through to the stale-entry path and is refreshed; guessing at an
// arbitrary regex's coverage would be worse than being wrong loudly.
func copilotHookMatcherIsNarrowed(entry any, want map[string]any) bool {
	fields, ok := entry.(map[string]any)
	if !ok {
		return false
	}
	got, gotOK := matcherAlternation(fields["matcher"])
	full, wantOK := matcherAlternation(want["matcher"])
	if !gotOK || !wantOK || len(got) == 0 || len(got) >= len(full) {
		return false
	}
	allowed := make(map[string]bool, len(full))
	for _, name := range full {
		allowed[name] = true
	}
	for _, name := range got {
		if !allowed[name] {
			return false
		}
	}
	return true
}

// matcherAlternation splits `^(?:a|b|c)$` into its alternatives.
func matcherAlternation(v any) ([]string, bool) {
	s, ok := v.(string)
	if !ok {
		return nil, false
	}
	inner, ok := strings.CutPrefix(s, "^(?:")
	if !ok {
		return nil, false
	}
	inner, ok = strings.CutSuffix(inner, ")$")
	if !ok {
		return nil, false
	}
	parts := strings.Split(inner, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			return nil, false
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out, true
}

// installHooks writes ~/.copilot/hooks/gortex.json. Callers gate this on
// env.InstallHooks and ModeGlobal.
func installHooks(env agents.Env, opts agents.ApplyOpts) (agents.FileAction, error) {
	path := HookConfigPath(env)
	action, err := agents.MergeJSON(env.Stderr, path, func(root map[string]any, _ bool) (bool, error) {
		return upsertCopilotHooks(root, env, opts), nil
	}, opts)
	if err != nil {
		return agents.FileAction{}, err
	}
	if action.Action != agents.ActionSkip {
		action.Keys = []string{"hooks"}
	}
	return action, nil
}
