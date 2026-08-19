package codex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

// inspect.go is the read-only counterpart to Apply: it reports what is
// actually on disk so `gortex doctor` can compare intent against reality.
// It reuses the same hook-entry predicates the writer uses, so a hook this
// package would install and a hook it recognises can never drift apart.

// InstallState is the observed state of the Codex integration.
//
// It answers "what did we write", not "is it working" — Codex gates hook
// execution behind per-hook trust, so a fully populated config here says
// nothing about whether any of it runs. That question is answered by the
// hook invocation log, and the two are only meaningful together.
type InstallState struct {
	ConfigPath    string `json:"config_path"`
	ConfigPresent bool   `json:"config_present"`
	MCPServer     bool   `json:"mcp_server"`
	// HooksPath is the hooks.json Codex reads alongside config.toml.
	HooksPath string `json:"hooks_path,omitempty"`
	// Hooks is the per-event count merged across every source in
	// HookSources — the number of Gortex entries Codex ends up running.
	Hooks map[string]int `json:"hooks"`
	// HookSources is the per-file breakdown behind Hooks, in the order Codex
	// discovers the files. Callers that act on a file must read this rather
	// than Hooks: acting on config.toml because of entries that live in
	// hooks.json is how a merged total misleads.
	HookSources []HookSource `json:"hook_sources,omitempty"`
	// HooksDisabled reports the `[features] hooks = false` opt-out, which
	// stops every hook above from running no matter how well it is declared.
	HooksDisabled bool `json:"hooks_disabled"`
	// InstructionsPath is the file Codex will actually load; ShadowedBy is
	// set when an override file takes precedence over the one Gortex writes,
	// which turns a correctly-installed rule block into dead text.
	InstructionsPath  string `json:"instructions_path,omitempty"`
	InstructionsWired bool   `json:"instructions_wired"`
	ShadowedBy        string `json:"shadowed_by,omitempty"`
}

// HookSource is one file Codex reads hook declarations from, with the
// Gortex-authored entries found in it.
//
// Codex takes hooks from a hooks.json *and* from inline [hooks] tables in
// config.toml, and merges the two when both are in the same layer — so every
// hook declared twice runs twice. Counting per file is what lets a caller see
// that, and what keeps "Gortex wrote these" (config.toml, the only
// representation this package writes) apart from "the user wrote these".
type HookSource struct {
	Path string `json:"path"`
	// Format is "json" for a hooks.json and "toml" for inline [hooks].
	Format string         `json:"format"`
	Hooks  map[string]int `json:"hooks"`
	Total  int            `json:"total"`
}

// HookCountIn reports the Gortex hooks declared in one specific file.
func (s InstallState) HookCountIn(path string) int {
	for _, src := range s.HookSources {
		if src.Path == path {
			return src.Total
		}
	}
	return 0
}

// HookEvents are the lifecycle events this adapter installs, in the order
// they matter for adoption: SessionStart is what states the Gortex rule for
// the session, so its absence explains a silent integration on its own.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

// TrustRemedy is how a user approves hooks Codex is skipping.
const TrustRemedy = "run `/hooks` inside Codex, review the gortex entries, and trust them"

// HooksDisabledRemedy is how a user turns Codex hook execution back on.
const HooksDisabledRemedy = "set `hooks = true` under `[features]` in ~/.codex/config.toml"

// UserHookFiles names the user-layer files Codex reads hook declarations
// from, in discovery order.
//
// Codex also reads a repo layer (<repo>/.codex/hooks.json and
// <repo>/.codex/config.toml). That is deliberately outside this package's
// scope: Inspect answers "what is installed for this user", and folding in a
// repo-local file would make that answer depend on whichever directory doctor
// happened to run in.
func UserHookFiles(home string) (hooksJSON, configTOML string) {
	return filepath.Join(home, ".codex", "hooks.json"),
		filepath.Join(home, ".codex", "config.toml")
}

// Inspect reads the Codex config and instructions file under home. Every
// failure degrades to "not configured" rather than an error: doctor's job is
// to describe a broken machine, not to fail on one.
//
// Hooks are counted from every user-layer file Codex reads them from, not
// only from the inline [hooks] tables this package writes. Reading one of the
// two representations and reporting the other as absent turns a healthy
// JSON-configured machine into a false alarm — and into advice to install a
// second, duplicate representation.
func Inspect(home string) InstallState {
	state := InstallState{Hooks: map[string]int{}}
	if home == "" {
		return state
	}
	for _, event := range HookEvents {
		state.Hooks[event] = 0
	}
	state.HooksPath, state.ConfigPath = UserHookFiles(home)

	if src, ok := inspectJSONHookFile(state.HooksPath); ok {
		state.HookSources = append(state.HookSources, src)
	}

	if data, err := os.ReadFile(state.ConfigPath); err == nil {
		state.ConfigPresent = true
		root := map[string]any{}
		if toml.Unmarshal(data, &root) == nil {
			if servers, ok := root["mcp_servers"].(map[string]any); ok {
				_, state.MCPServer = servers["gortex"]
			}
			state.HooksDisabled = hooksFeatureDisabled(root)
			state.HookSources = append(state.HookSources, hookSourceFrom(state.ConfigPath, "toml", root))
		}
	}

	for _, src := range state.HookSources {
		for event, n := range src.Hooks {
			state.Hooks[event] += n
		}
	}

	// Codex prefers the override file in its home and then ignores AGENTS.md
	// entirely, so report the file it will really read.
	base := filepath.Join(home, ".codex", codexGlobalInstructionsFile)
	override := filepath.Join(home, ".codex", codexGlobalInstructionsOverrideFile)
	state.InstructionsPath = base
	if _, err := os.Stat(override); err == nil {
		if _, err := os.Stat(base); err == nil {
			state.ShadowedBy = override
		}
		state.InstructionsPath = override
	}
	if body, err := os.ReadFile(state.InstructionsPath); err == nil {
		text := string(body)
		state.InstructionsWired = strings.Contains(text, "gortex:rules:start") ||
			strings.Contains(text, "MANDATORY: Use Gortex MCP")
	}
	return state
}

// inspectJSONHookFile reads a Codex hooks.json.
//
// Its root carries the same "hooks" table keyed by event name that config.toml
// does, so the writer's own recognisers apply unchanged. The bool reports only
// whether the file exists — a file that exists but does not parse is returned
// with no entries, which is also what Codex will run from it.
func inspectJSONHookFile(path string) (HookSource, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HookSource{}, false
	}
	src := HookSource{Path: path, Format: "json", Hooks: map[string]int{}}
	root := map[string]any{}
	if json.Unmarshal(data, &root) != nil {
		return src, true
	}
	countGortexHooks(root, src.Hooks)
	src.Total = totalHooks(src.Hooks)
	return src, true
}

// hookSourceFrom tallies one already-parsed configuration root.
func hookSourceFrom(path, format string, root map[string]any) HookSource {
	src := HookSource{Path: path, Format: format, Hooks: map[string]int{}}
	countGortexHooks(root, src.Hooks)
	src.Total = totalHooks(src.Hooks)
	return src
}

// hooksFeatureDisabled reports the `[features] hooks = false` opt-out. Codex
// runs hooks by default, so only an explicit false counts — an absent key is
// not a disabled feature.
func hooksFeatureDisabled(root map[string]any) bool {
	features, ok := root["features"].(map[string]any)
	if !ok {
		return false
	}
	enabled, ok := features["hooks"].(bool)
	return ok && !enabled
}

// totalHooks sums a per-event count.
func totalHooks(hooks map[string]int) int {
	total := 0
	for _, n := range hooks {
		total += n
	}
	return total
}

// countGortexHooks tallies Gortex-authored entries per event, reusing the
// writer's own recognisers so a hook shape we install is always a hook shape
// we can find again.
func countGortexHooks(root map[string]any, out map[string]int) {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return
	}
	recognisers := map[string]func(any) bool{
		"SessionStart":     codexHookEntryIsGortexSessionStart,
		"UserPromptSubmit": codexHookEntryIsGortexUserPromptSubmit,
		"PreToolUse":       codexHookEntryIsGortexPreToolUse,
		"PostToolUse":      codexHookEntryIsGortexPostToolUse,
		"Stop":             codexHookEntryIsGortexStop,
	}
	for event, isGortex := range recognisers {
		entries, ok := codexHookList(hooks[event])
		if !ok {
			continue
		}
		for _, entry := range entries {
			if isGortex(entry) {
				out[event]++
			}
		}
	}
}
