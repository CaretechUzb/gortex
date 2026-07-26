package claudecode

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/zzet/gortex/internal/agents"
)

// inspect.go is the read-only counterpart to Apply, mirroring codex.Inspect so
// `gortex doctor` can report every agent the same way instead of singling one
// out. It reuses this package's own path helpers, so a file the adapter writes
// and a file the reporter looks for cannot drift apart.

// InstallState is the observed state of the Claude Code integration.
type InstallState struct {
	ConfigPath        string         `json:"config_path"`
	ConfigPresent     bool           `json:"config_present"`
	MCPServer         bool           `json:"mcp_server"`
	Hooks             map[string]int `json:"hooks"`
	InstructionsPath  string         `json:"instructions_path,omitempty"`
	InstructionsWired bool           `json:"instructions_wired"`
}

// HookEvents are the lifecycle events this adapter installs. SessionStart
// leads for the same reason it does everywhere: it is what states the Gortex
// rule for the session.
var HookEvents = []string{"SessionStart", "UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}

// Inspect reads the user-level Claude Code config. Every failure degrades to
// "not configured" rather than an error — doctor's job is to describe a broken
// machine, not to fail on one.
func Inspect(home string) InstallState {
	state := InstallState{Hooks: map[string]int{}}
	if home == "" {
		return state
	}
	for _, event := range HookEvents {
		state.Hooks[event] = 0
	}

	// Hooks live in settings.local.json; the MCP server stanza lives in
	// ~/.claude.json. Report the hooks file as the config path, since that is
	// the one whose contents the runtime section is about.
	state.ConfigPath = userSettingsLocalPath(home)
	if data, err := os.ReadFile(state.ConfigPath); err == nil {
		state.ConfigPresent = true
		var settings struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if json.Unmarshal(data, &settings) == nil {
			for event, entries := range settings.Hooks {
				if _, tracked := state.Hooks[event]; !tracked {
					continue
				}
				for _, entry := range entries {
					for _, h := range entry.Hooks {
						// Same recogniser the installer's idempotency check
						// uses: a gortex-invoking command, however it is
						// spelled or pathed.
						if strings.Contains(h.Command, "gortex") {
							state.Hooks[event]++
							break
						}
					}
				}
			}
		}
	}

	if data, err := os.ReadFile(userClaudeJSONPath(home)); err == nil {
		var root struct {
			MCPServers map[string]any `json:"mcpServers"`
		}
		if json.Unmarshal(data, &root) == nil {
			_, state.MCPServer = root.MCPServers["gortex"]
		}
	}

	state.InstructionsPath = UserClaudeMdPath(home)
	if body, err := os.ReadFile(state.InstructionsPath); err == nil {
		text := string(body)
		state.InstructionsWired = strings.Contains(text, agents.GlobalRulesStartMarker) ||
			strings.Contains(text, agents.InstructionsSentinel)
	}
	return state
}
