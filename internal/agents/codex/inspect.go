package codex

import (
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
	ConfigPath    string         `json:"config_path"`
	ConfigPresent bool           `json:"config_present"`
	MCPServer     bool           `json:"mcp_server"`
	Hooks         map[string]int `json:"hooks"`
	// InstructionsPath is the file Codex will actually load; ShadowedBy is
	// set when an override file takes precedence over the one Gortex writes,
	// which turns a correctly-installed rule block into dead text.
	InstructionsPath  string `json:"instructions_path,omitempty"`
	InstructionsWired bool   `json:"instructions_wired"`
	ShadowedBy        string `json:"shadowed_by,omitempty"`
}

// HookEvents are the lifecycle events this adapter installs, in the order
// they matter for adoption: SessionStart is what states the Gortex rule for
// the session, so its absence explains a silent integration on its own.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

// TrustRemedy is how a user approves hooks Codex is skipping.
const TrustRemedy = "run `/hooks` inside Codex, review the gortex entries, and trust them"

// Inspect reads the Codex config and instructions file under home. Every
// failure degrades to "not configured" rather than an error: doctor's job is
// to describe a broken machine, not to fail on one.
func Inspect(home string) InstallState {
	state := InstallState{Hooks: map[string]int{}}
	if home == "" {
		return state
	}
	for _, event := range HookEvents {
		state.Hooks[event] = 0
	}
	state.ConfigPath = filepath.Join(home, ".codex", "config.toml")

	if data, err := os.ReadFile(state.ConfigPath); err == nil {
		state.ConfigPresent = true
		root := map[string]any{}
		if toml.Unmarshal(data, &root) == nil {
			if servers, ok := root["mcp_servers"].(map[string]any); ok {
				_, state.MCPServer = servers["gortex"]
			}
			countGortexHooks(root, state.Hooks)
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
