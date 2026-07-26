package claudecode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestInspectSeesWhatApplyWrote is the drift fence between the writer and the
// reader, mirroring the codex adapter's. `gortex doctor` reports "hooks
// configured" from Inspect; if applyGlobal's hook shape changes without
// Inspect following, doctor would report a correct install as unconfigured —
// turning the diagnostic into a second bug rather than a tool for the first.
func TestInspectSeesWhatApplyWrote(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.Mode = agents.ModeGlobal
	env.InstallGlobalInstructions = true
	env.InstallHooks = true

	before := Inspect(env.Home)
	if before.ConfigPresent || before.MCPServer || before.InstructionsWired {
		t.Fatalf("fixture home should start empty: %+v", before)
	}

	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	after := Inspect(env.Home)
	if !after.ConfigPresent {
		t.Error("settings.local.json not seen after install")
	}
	if !after.MCPServer {
		t.Error("mcpServers.gortex not seen after install")
	}
	if !after.InstructionsWired {
		t.Error("the CLAUDE.md rule block is not visible to Inspect")
	}
	var wired int
	for _, event := range HookEvents {
		wired += after.Hooks[event]
	}
	if wired == 0 {
		t.Errorf("Apply wrote hooks that Inspect cannot see: %+v", after.Hooks)
	}
}

// TestInspectIgnoresForeignHooks keeps doctor from crediting Gortex for
// someone else's hook, which would mask a missing Gortex one.
func TestInspectIgnoresForeignHooks(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	path := userSettingsLocalPath(env.Home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"hooks":{"SessionStart":[{"hooks":[{"type":"command","command":"/usr/local/bin/other-tool notify"}]}]}}`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	state := Inspect(env.Home)
	if !state.ConfigPresent {
		t.Fatal("settings.local.json not seen")
	}
	if state.Hooks["SessionStart"] != 0 {
		t.Errorf("counted a non-Gortex hook: %d", state.Hooks["SessionStart"])
	}
}

// TestInspectSurvivesBrokenConfig — doctor exists to describe broken machines.
func TestInspectSurvivesBrokenConfig(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	path := userSettingsLocalPath(env.Home)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := Inspect(env.Home)
	if !state.ConfigPresent {
		t.Error("a malformed config still exists on disk")
	}
	if state.MCPServer {
		t.Error("nothing parseable, so nothing should be claimed")
	}

	if got := Inspect(""); got.ConfigPresent {
		t.Errorf("empty home should report nothing, got %+v", got)
	}
}
