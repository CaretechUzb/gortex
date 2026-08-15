package copilotcli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// TestInspectRoundTripsWhatApplyWrote is the fence that keeps the writer
// and the reporter from drifting: doctor must find the entries this
// package just installed.
func TestInspectRoundTripsWhatApplyWrote(t *testing.T) {
	env, _ := globalEnv(t)
	env.InstallGlobalInstructions = true
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	state := Inspect(env.Home)
	if state.ConfigPath != HookConfigPath(env) {
		t.Fatalf("ConfigPath = %q, want %q", state.ConfigPath, HookConfigPath(env))
	}
	if !state.ConfigPresent {
		t.Fatal("ConfigPresent = false after Apply wrote the hook config")
	}
	if !state.MCPServer {
		t.Fatal("MCPServer = false after Apply registered the gortex server")
	}
	if state.InstructionsPath != GlobalInstructionsPath(env) {
		t.Fatalf("InstructionsPath = %q, want %q", state.InstructionsPath, GlobalInstructionsPath(env))
	}
	if !state.InstructionsWired {
		t.Fatal("InstructionsWired = false after Apply wrote the rule block")
	}
	for _, event := range HookEvents {
		if state.Hooks[event] != 1 {
			t.Errorf("Hooks[%s] = %d, want 1", event, state.Hooks[event])
		}
	}
	if len(state.Hooks) != len(HookEvents) {
		t.Errorf("Hooks has %d keys, want %d: %v", len(state.Hooks), len(HookEvents), state.Hooks)
	}
}

// TestInspectKeysAreClaudePascalCase is the whole reason inspect.go
// translates at all. `gortex doctor` tallies hook runs against
// internal/hooks' PascalCase-only allowlist, so a Hooks map keyed with
// Copilot's own camelCase spellings scores every event ran==0 — and
// doctor then prints a permanent "configured but none has run" blocker
// over a perfectly working install.
func TestInspectKeysAreClaudePascalCase(t *testing.T) {
	env, _ := globalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	want := map[string]bool{
		"SessionStart": true, "UserPromptSubmit": true,
		"PreToolUse": true, "PostToolUse": true,
	}
	state := Inspect(env.Home)
	for key := range state.Hooks {
		if !want[key] {
			t.Errorf("Hooks key %q is not Claude's PascalCase vocabulary", key)
		}
	}
	// The on-disk spellings must never leak out of the writer.
	for _, onDisk := range copilotHookEventNames {
		if _, leaked := state.Hooks[onDisk]; leaked {
			t.Errorf("Hooks leaked the on-disk spelling %q", onDisk)
		}
	}
	// And they must actually differ, or the test proves nothing.
	if copilotHookEventNames["UserPromptSubmit"] != "userPromptSubmitted" {
		t.Fatalf("the CLI's event name is past tense: got %q", copilotHookEventNames["UserPromptSubmit"])
	}
}

// TestInspectDegradesOnAnEmptyMachine: doctor's job is to describe a
// broken machine, not to fail on one.
func TestInspectDegradesOnAnEmptyMachine(t *testing.T) {
	t.Setenv(copilotHomeEnvVar, "")
	if state := Inspect(""); state.ConfigPresent || len(state.Hooks) != 0 {
		t.Fatalf("empty home must yield a zero state, got %+v", state)
	}
	state := Inspect(t.TempDir())
	if state.ConfigPresent || state.MCPServer || state.InstructionsWired {
		t.Fatalf("unconfigured home must report nothing installed, got %+v", state)
	}
	for _, event := range HookEvents {
		if state.Hooks[event] != 0 {
			t.Errorf("Hooks[%s] = %d on an unconfigured machine", event, state.Hooks[event])
		}
	}
}

// TestInspectFindsAHandWrittenGortexHook: recognition is by command
// content, so an entry written by an older gortex (or copied by a user)
// still counts.
func TestInspectFindsAHandWrittenGortexHook(t *testing.T) {
	env, _ := globalEnv(t)
	seedHooks(t, env, map[string]any{
		"version":         1,
		"disableAllHooks": false,
		"hooks": map[string]any{
			"postToolUse": []any{map[string]any{
				"type": "command", "matcher": "^(?:bash)$",
				"bash": "gortex hook --agent copilot-cli",
			}},
			// A user hook that is not ours must not be counted.
			"sessionStart": []any{map[string]any{"type": "command", "bash": "echo hi"}},
		},
	})

	state := Inspect(env.Home)
	if state.Hooks["PostToolUse"] != 1 {
		t.Errorf("PostToolUse = %d, want the space-separated --agent form recognised", state.Hooks["PostToolUse"])
	}
	if state.Hooks["SessionStart"] != 0 {
		t.Errorf("SessionStart = %d, want a user hook not counted as ours", state.Hooks["SessionStart"])
	}
}

// TestInspectHonoursTheSandboxHomeGuard mirrors the adapter's own
// COPILOT_HOME rule: an exported override must not redirect Inspect onto
// the developer's real config while doctor is probing a sandbox home.
func TestInspectHonoursTheSandboxHomeGuard(t *testing.T) {
	t.Setenv(copilotHomeEnvVar, t.TempDir())
	home := t.TempDir()
	state := Inspect(home)
	want := filepath.Join(home, copilotConfigDirName, hooksDirName, hookFileName)
	if state.ConfigPath != want {
		t.Fatalf("ConfigPath = %q, want %q", state.ConfigPath, want)
	}
	if strings.Contains(state.InstructionsPath, "hooks") {
		t.Fatalf("InstructionsPath = %q", state.InstructionsPath)
	}
}
