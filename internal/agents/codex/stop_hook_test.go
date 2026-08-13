package codex

import (
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

func TestCodexInstallsAndInspectsStopHook(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallGlobalInstructions = true
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertCodexStopHook(t, readCodexConfig(t, env))

	state := Inspect(env.Home)
	if got := state.Hooks["Stop"]; got != 1 {
		t.Fatalf("doctor counted Stop hooks=%d want 1: %+v", got, state.Hooks)
	}
}

func TestCodexInstallHooksOnlyIncludesStop(t *testing.T) {
	env := codexGlobalEnv(t)
	path := filepath.Join(env.Home, ".codex", "config.toml")
	if _, err := InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("install hooks only: %v", err)
	}
	assertCodexStopHook(t, readCodexConfig(t, env))
}

func assertCodexStopHook(t *testing.T, cfg map[string]any) {
	t.Helper()
	entries := hookEntries(t, cfg, "Stop")
	if len(entries) != 1 {
		t.Fatalf("Stop entries=%d want 1: %#v", len(entries), entries)
	}
	entry, ok := entries[0].(map[string]any)
	if !ok {
		t.Fatalf("Stop entry has unexpected shape: %#v", entries[0])
	}
	if _, hasMatcher := entry["matcher"]; hasMatcher {
		t.Fatalf("Stop entry should carry no matcher: %#v", entry)
	}
	if !codexHookEntryIsGortexStop(entry) {
		t.Fatalf("installed Stop entry is not recognized as Gortex: %#v", entry)
	}
	handlers, ok := codexHookList(entry["hooks"])
	if !ok || len(handlers) != 1 {
		t.Fatalf("Stop handlers=%#v", entry["hooks"])
	}
	handler := handlers[0].(map[string]any)
	if handler["command"] != testCodexHookCommand {
		t.Errorf("command=%v want %q", handler["command"], testCodexHookCommand)
	}
	if handler["timeout"] != int64(codexHookTimeoutSeconds) {
		t.Errorf("timeout=%v want %d", handler["timeout"], codexHookTimeoutSeconds)
	}
}
