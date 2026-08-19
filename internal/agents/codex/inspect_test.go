package codex

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// writeCodexHooksJSON writes the hooks.json Codex reads alongside config.toml.
// The shape is Codex's documented one: a "hooks" table keyed by event name,
// each event holding matcher groups whose "hooks" list carries the handlers.
func writeCodexHooksJSON(t *testing.T, home, command string) string {
	t.Helper()
	path := filepath.Join(home, ".codex", "hooks.json")
	body := `{
  "description": "hooks for this machine",
  "hooks": {
    "SessionStart": [
      {
        "hooks": [
          {"type": "command", "command": "` + command + `", "timeout": 5}
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "` + command + `", "timeout": 5}
        ]
      }
    ]
  }
}
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestInspectCountsHooksDeclaredInJSON is the reported false negative. Codex
// reads hooks from hooks.json as well as from inline [hooks] tables, and
// reading only the latter reported a working JSON-configured machine as
// having no hooks at all — which then recommended installing a second,
// duplicate representation of the hooks it already had.
func TestInspectCountsHooksDeclaredInJSON(t *testing.T) {
	env := codexGlobalEnv(t)
	hooksPath := writeCodexHooksJSON(t, env.Home, "gortex hook --agent=codex --mode=enrich")

	state := Inspect(env.Home)

	if state.Hooks["SessionStart"] != 1 {
		t.Errorf("SessionStart=%d, want 1 — the hook is declared in %s", state.Hooks["SessionStart"], hooksPath)
	}
	if state.Hooks["PreToolUse"] != 1 {
		t.Errorf("PreToolUse=%d, want 1", state.Hooks["PreToolUse"])
	}
	if state.HooksPath != hooksPath {
		t.Errorf("HooksPath=%q want %q", state.HooksPath, hooksPath)
	}
	if got := state.HookCountIn(hooksPath); got != 2 {
		t.Errorf("HookCountIn(hooks.json)=%d, want 2", got)
	}
	if len(state.HookSources) != 1 || state.HookSources[0].Path != hooksPath {
		t.Errorf("HookSources=%+v, want just %s", state.HookSources, hooksPath)
	}
}

// TestInspectMergesHookSources pins the counts Codex actually ends up with
// when both representations exist, and keeps the per-file breakdown intact so
// a caller can still tell them apart.
func TestInspectMergesHookSources(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallGlobalInstructions = true
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	hooksPath := writeCodexHooksJSON(t, env.Home, "gortex hook --agent=codex --mode=enrich")
	configPath := codexConfigPath(env)

	state := Inspect(env.Home)

	if state.Hooks["SessionStart"] != 2 {
		t.Errorf("SessionStart=%d, want 2 (one per file) — Codex merges same-layer hook files", state.Hooks["SessionStart"])
	}
	if got := state.HookCountIn(hooksPath); got != 2 {
		t.Errorf("HookCountIn(hooks.json)=%d, want 2", got)
	}
	if got := state.HookCountIn(configPath); got == 0 {
		t.Error("the inline [hooks] Apply wrote are not attributed to config.toml")
	}
	if len(state.HookSources) != 2 {
		t.Fatalf("HookSources=%+v, want both files", state.HookSources)
	}
	if state.HookSources[0].Path != hooksPath || state.HookSources[1].Path != configPath {
		t.Errorf("HookSources lists %s then %s, want hooks.json first (Codex discovery order)",
			state.HookSources[0].Path, state.HookSources[1].Path)
	}
}

// TestInspectIgnoresForeignJSONHooks keeps the JSON reader as strict as the
// TOML one: crediting Gortex for somebody else's hook would mask a missing one.
func TestInspectIgnoresForeignJSONHooks(t *testing.T) {
	env := codexGlobalEnv(t)
	hooksPath := writeCodexHooksJSON(t, env.Home, "/usr/local/bin/some-other-tool notify")

	state := Inspect(env.Home)

	if got := totalHooks(state.Hooks); got != 0 {
		t.Errorf("counted %d foreign hook(s) as Gortex's", got)
	}
	if got := state.HookCountIn(hooksPath); got != 0 {
		t.Errorf("HookCountIn(hooks.json)=%d, want 0", got)
	}
	if state.HookSources[0].Total != 0 {
		t.Errorf("hooks.json source Total=%d, want 0", state.HookSources[0].Total)
	}
}

// TestInspectSurvivesBrokenHooksJSON — a hooks.json Codex cannot parse runs no
// hooks, so reporting none from it is the effective truth, and it must not
// take the rest of the report down with it.
func TestInspectSurvivesBrokenHooksJSON(t *testing.T) {
	env := codexGlobalEnv(t)
	env.InstallGlobalInstructions = true
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	hooksPath := filepath.Join(env.Home, ".codex", "hooks.json")
	if err := os.WriteFile(hooksPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	state := Inspect(env.Home)

	if got := state.HookCountIn(hooksPath); got != 0 {
		t.Errorf("HookCountIn(broken hooks.json)=%d, want 0", got)
	}
	if state.Hooks["SessionStart"] == 0 {
		t.Error("a broken hooks.json hid the hooks config.toml really has")
	}
	if !state.MCPServer {
		t.Error("a broken hooks.json must not affect the rest of the report")
	}
}

// TestInspectReportsHooksTurnedOff covers the mirror-image of the missing
// count: hooks are declared, and Codex is configured not to run any of them.
func TestInspectReportsHooksTurnedOff(t *testing.T) {
	env := codexGlobalEnv(t)
	path := codexConfigPath(env)
	if err := os.WriteFile(path, []byte("[features]\nhooks = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if state := Inspect(env.Home); !state.HooksDisabled {
		t.Error("`[features] hooks = false` turns hook execution off and must be reported")
	}

	if err := os.WriteFile(path, []byte("[features]\nhooks = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if state := Inspect(env.Home); state.HooksDisabled {
		t.Error("hooks = true is not disabled")
	}

	if err := os.WriteFile(path, []byte("[features]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if state := Inspect(env.Home); state.HooksDisabled {
		t.Error("Codex runs hooks by default, so an absent key is not a disabled feature")
	}
}

// TestGlobalArtifactsStaysScopedToTheFileItEdits — RemoveGlobal only rewrites
// config.toml, so listing it because hooks.json carries Gortex entries would
// promise the uninstall wizard a deletion that never happens.
func TestGlobalArtifactsStaysScopedToTheFileItEdits(t *testing.T) {
	env := codexGlobalEnv(t)
	configPath := codexConfigPath(env)
	if err := os.WriteFile(configPath, []byte("model = \"gpt-5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCodexHooksJSON(t, env.Home, "gortex hook --agent=codex --mode=enrich")

	if state := Inspect(env.Home); totalHooks(state.Hooks) == 0 {
		t.Fatal("fixture is wrong: the hooks.json entries should be counted")
	}
	for _, path := range GlobalArtifacts(env.Home) {
		if path == configPath {
			t.Fatalf("config.toml listed for removal, but the Gortex hooks are in hooks.json: %v", GlobalArtifacts(env.Home))
		}
	}
}

// TestApplyWarnsWhenItWouldDuplicateHooksJSON — the installer still writes its
// own inline representation (the hooks.json entries are the user's), but Codex
// merges the two and then runs each hook twice, so it has to say so.
func TestApplyWarnsWhenItWouldDuplicateHooksJSON(t *testing.T) {
	env := codexGlobalEnv(t)
	hooksPath := writeCodexHooksJSON(t, env.Home, "gortex hook --agent=codex --mode=enrich")

	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	var notice string
	for _, w := range res.Warnings {
		if strings.Contains(w, hooksPath) {
			notice = w
		}
	}
	if notice == "" {
		t.Fatalf("no warning naming %s in %v", hooksPath, res.Warnings)
	}
	if !strings.Contains(notice, "twice") {
		t.Errorf("the warning must name the consequence, got %q", notice)
	}

	// A machine without a hooks.json must stay quiet.
	clean := codexGlobalEnv(t)
	cleanRes, err := New().Apply(clean, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, w := range cleanRes.Warnings {
		if strings.Contains(w, "hooks.json") {
			t.Errorf("unexpected duplicate warning with no hooks.json on disk: %q", w)
		}
	}
}

// TestInstallHooksOnlyWarnsAboutTheSameDuplicate — `gortex init --hooks-only`
// writes the same inline tables Apply does, through a different entry point,
// so it can create the same silent duplicate and has to say the same thing.
func TestInstallHooksOnlyWarnsAboutTheSameDuplicate(t *testing.T) {
	env := codexGlobalEnv(t)
	hooksPath := writeCodexHooksJSON(t, env.Home, "gortex hook --agent=codex --mode=enrich")
	configPath := codexConfigPath(env)

	var out bytes.Buffer
	if _, err := InstallHooksOnly(&out, configPath, env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("install hooks: %v", err)
	}
	if !strings.Contains(out.String(), hooksPath) {
		t.Errorf("no duplicate warning naming %s:\n%s", hooksPath, out.String())
	}
	if !strings.Contains(out.String(), "twice") {
		t.Errorf("the warning must name the consequence:\n%s", out.String())
	}

	// Without a hooks.json there is no duplicate to warn about.
	clean := codexGlobalEnv(t)
	var quiet bytes.Buffer
	if _, err := InstallHooksOnly(&quiet, codexConfigPath(clean), clean, agents.ApplyOpts{}); err != nil {
		t.Fatalf("install hooks: %v", err)
	}
	if strings.Contains(quiet.String(), "hooks.json") {
		t.Errorf("unexpected duplicate warning:\n%s", quiet.String())
	}
}

// TestDuplicateNoticeFollowsTheConfigLayer — Codex merges hook files per
// layer, so the notice must compare a config.toml against the hooks.json
// beside it, not against the user's.
func TestDuplicateNoticeFollowsTheConfigLayer(t *testing.T) {
	env := codexGlobalEnv(t)
	writeCodexHooksJSON(t, env.Home, "gortex hook --agent=codex --mode=enrich")

	repoConfig := filepath.Join(t.TempDir(), ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(repoConfig), 0o755); err != nil {
		t.Fatal(err)
	}
	if notice := codexDuplicateHookFileNotice(repoConfig); notice != "" {
		t.Errorf("the user's hooks.json does not merge with a repo-local config.toml: %q", notice)
	}
	if notice := codexDuplicateHookFileNotice(codexConfigPath(env)); notice == "" {
		t.Error("the hooks.json beside the config does merge with it")
	}
}
