package copilotcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/zzet/gortex/internal/agents"
)

// inspect.go is the read-only counterpart to Apply: it reports what is
// actually on disk so `gortex doctor` can compare intent against reality.
// It reuses the writer's own recogniser, so a hook this package installs
// and a hook it can find again never drift apart.

// InstallState is the observed state of the Copilot CLI integration.
//
// # Stop-line: no adoption / session scan
//
// Every other agent doctor covers pairs this with a transcript scan
// ("what did the model actually call?"). Copilot CLI gets none. Its
// session history lives in ~/.copilot/session-store.db — SQLite with an
// undocumented, unversioned schema, no export path, and no compatibility
// promise. Reading it would mean reverse-engineering private tables that
// can change in any patch release, and a scanner that silently starts
// reporting zeros after an upgrade is worse than one that never existed.
// Doctor reports install state and hook activity for this agent and stops
// there; the Adoption slice stays at its zero value on purpose.
type InstallState struct {
	ConfigPath        string         `json:"config_path"`
	ConfigPresent     bool           `json:"config_present"`
	MCPServer         bool           `json:"mcp_server"`
	Hooks             map[string]int `json:"hooks"`
	InstructionsPath  string         `json:"instructions_path,omitempty"`
	InstructionsWired bool           `json:"instructions_wired"`
}

// HookEvents are the lifecycle events this adapter installs, in the order
// they matter for adoption: SessionStart is what states the Gortex rule
// for the session, so its absence explains a silent integration on its
// own.
//
// These are Claude's PascalCase spellings, NOT the camelCase names that
// go on disk (see copilotHookEventNames). internal/hooks' effectiveness
// log and doctor's tally are keyed on the PascalCase vocabulary and
// ignore anything else, so keying this list — or the Hooks map below —
// with Copilot's own spellings would score every event ran==0 and make
// doctor print a permanent "configured but none has run" blocker over a
// perfectly working install.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse"}

// Inspect reads the Copilot CLI config under home. Every failure degrades
// to "not configured" rather than an error: doctor's job is to describe a
// broken machine, not to fail on one.
func Inspect(home string) InstallState {
	state := InstallState{Hooks: map[string]int{}}
	if home == "" {
		return state
	}
	for _, event := range HookEvents {
		state.Hooks[event] = 0
	}

	env := agents.Env{Home: home}
	configHome := copilotConfigHome(env)
	if configHome == "" {
		return state
	}

	// The hooks file is reported as the config path: it is the one whose
	// contents doctor's runtime section is about. The MCP registry is a
	// separate file and is only probed for the server's presence.
	state.ConfigPath = filepath.Join(configHome, hooksDirName, hookFileName)
	if data, err := os.ReadFile(state.ConfigPath); err == nil {
		state.ConfigPresent = true
		var root map[string]any
		if json.Unmarshal(data, &root) == nil {
			countGortexCopilotHooks(root, state.Hooks)
		}
	}

	if data, err := os.ReadFile(filepath.Join(configHome, mcpConfigFileName)); err == nil {
		var root struct {
			MCPServers map[string]any `json:"mcpServers"`
		}
		if json.Unmarshal(data, &root) == nil {
			_, state.MCPServer = root.MCPServers[serverName]
		}
	}

	state.InstructionsPath = filepath.Join(configHome, instructionsFileName)
	if body, err := os.ReadFile(state.InstructionsPath); err == nil {
		text := string(body)
		state.InstructionsWired = strings.Contains(text, agents.GlobalRulesStartMarker) ||
			strings.Contains(text, agents.InstructionsSentinel)
	}
	return state
}

// countGortexCopilotHooks tallies Gortex-authored entries per event. The
// on-disk keys are camelCase; the output map is PascalCase, so the
// translation happens here and nowhere downstream.
func countGortexCopilotHooks(root map[string]any, out map[string]int) {
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		return
	}
	for _, event := range HookEvents {
		entries, ok := copilotHookList(hooks[copilotHookEventNames[event]])
		if !ok {
			continue
		}
		for _, entry := range entries {
			if isGortexCopilotHookEntry(entry) {
				out[event]++
			}
		}
	}
}
