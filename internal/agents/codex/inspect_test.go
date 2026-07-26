package codex

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// TestInspectSeesWhatApplyWrote is the drift fence between the writer and the
// reader. `gortex doctor` reports "hooks configured" from Inspect; if Apply's
// hook shape ever changes without Inspect following, doctor would quietly
// report a correctly-installed integration as unconfigured — turning the
// diagnostic into a second bug rather than a tool for finding the first.
func TestInspectSeesWhatApplyWrote(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallGlobalInstructions = true

	before := Inspect(env.Home)
	if before.ConfigPresent {
		t.Fatalf("fixture home should start empty, got %+v", before)
	}
	for _, event := range HookEvents {
		if before.Hooks[event] != 0 {
			t.Fatalf("%s counted %d hooks before install", event, before.Hooks[event])
		}
	}

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := Inspect(env.Home)
	if !after.ConfigPresent {
		t.Fatal("config.toml not seen after install")
	}
	if !after.MCPServer {
		t.Fatal("mcp_servers.gortex not seen after install")
	}
	for _, event := range HookEvents {
		if after.Hooks[event] == 0 {
			t.Errorf("%s: Apply wrote a hook that Inspect cannot see", event)
		}
	}
	if !after.InstructionsWired {
		t.Error("rule block written by Apply is not visible to Inspect")
	}
	if after.ShadowedBy != "" {
		t.Errorf("unexpected shadowing: %s", after.ShadowedBy)
	}
}

// TestInspectReportsOverrideShadowing pins the case where the rule block is
// installed correctly and still never reaches the model: Codex reads
// AGENTS.override.md in its home and ignores AGENTS.md entirely.
func TestInspectReportsOverrideShadowing(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallGlobalInstructions = true
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	override := filepath.Join(env.Home, ".codex", codexGlobalInstructionsOverrideFile)
	if err := os.WriteFile(override, []byte("# my own rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	state := Inspect(env.Home)
	if state.ShadowedBy != override {
		t.Fatalf("ShadowedBy=%q want %q", state.ShadowedBy, override)
	}
	if state.InstructionsPath != override {
		t.Fatalf("InstructionsPath=%q should be the file Codex actually reads", state.InstructionsPath)
	}
	if state.InstructionsWired {
		t.Error("the override has no Gortex block, so the rule is not wired")
	}
}

// TestInspectIgnoresForeignHooks keeps doctor from crediting Gortex for a
// hook somebody else installed — which would mask a missing Gortex hook.
func TestInspectIgnoresForeignHooks(t *testing.T) {
	env := codexGlobalEnv(t)
	path := filepath.Join(env.Home, ".codex", "config.toml")
	config := "" +
		"[[hooks.SessionStart]]\n" +
		"matcher = \"startup\"\n\n" +
		"[[hooks.SessionStart.hooks]]\n" +
		"type = \"command\"\n" +
		"command = \"/usr/local/bin/some-other-tool notify\"\n"
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	state := Inspect(env.Home)
	if !state.ConfigPresent {
		t.Fatal("config.toml not seen")
	}
	if state.Hooks["SessionStart"] != 0 {
		t.Fatalf("counted a non-Gortex hook: %d", state.Hooks["SessionStart"])
	}
}

// TestInspectSurvivesBrokenConfig — doctor exists to describe broken
// machines, so unreadable or malformed input must degrade, never panic.
func TestInspectSurvivesBrokenConfig(t *testing.T) {
	env := codexGlobalEnv(t)
	path := filepath.Join(env.Home, ".codex", "config.toml")
	if err := os.WriteFile(path, []byte("this is not { valid toml ][\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := Inspect(env.Home)
	if !state.ConfigPresent {
		t.Fatal("a malformed config still exists on disk")
	}
	if state.MCPServer {
		t.Error("nothing parseable, so nothing should be claimed")
	}

	if got := Inspect(""); got.ConfigPresent {
		t.Errorf("empty home should report nothing, got %+v", got)
	}
	if got := Inspect(filepath.Join(env.Home, "nope")); got.ConfigPresent {
		t.Errorf("missing home should report nothing, got %+v", got)
	}
}
