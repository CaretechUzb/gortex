package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// userGlobalOpenCodeConfig is the user's own ~/.config/opencode/opencode.json,
// carrying a server of their own. `gortex install` adds `mcp.gortex`
// alongside it and RemoveGlobal takes only that key back out — the file
// itself, the user's own server, and every other key must survive.
const userGlobalOpenCodeConfig = `{
  "$schema": "https://opencode.ai/config.json",
  "mcp": {
    "other": { "type": "local", "command": ["node", "server.js"], "enabled": true }
  }
}
`

func writeOpenCodeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// installedOpenCodeGlobal runs a ModeGlobal Apply over a home that already
// holds the user's own global config and skill, and returns the env plus
// the user skill's path. Starting from a real install rather than a
// hand-built fixture is what keeps the remover from asserting against a
// shape the writer no longer produces.
func installedOpenCodeGlobal(t *testing.T) (agents.Env, string) {
	t.Helper()
	env, _ := globalEnv(t)

	writeOpenCodeFile(t, filepath.Join(globalConfigDir(env.Home), "opencode.json"), userGlobalOpenCodeConfig)

	// A hand-written skill in the SHARED skills root — the whole reason
	// removal cannot just delete the directory.
	userSkill := filepath.Join(globalSkillsDir(env.Home), "team-runbook", skillFileName)
	writeOpenCodeFile(t, userSkill, "---\nname: team-runbook\n---\n\nOurs, not Gortex's.\n")

	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := GlobalArtifacts(env.Home); len(got) == 0 {
		t.Fatal("expected a non-empty footprint after install")
	}
	return env, userSkill
}

// TestOpenCodeRemoveGlobalLeavesNoGortexArtifact is the round-trip
// contract: after Apply then RemoveGlobal nothing of Gortex's is left,
// and every user-owned thing that shared a tree with it survives
// byte-identical.
func TestOpenCodeRemoveGlobalLeavesNoGortexArtifact(t *testing.T) {
	env, userSkill := installedOpenCodeGlobal(t)
	skillBefore, err := os.ReadFile(userSkill)
	if err != nil {
		t.Fatal(err)
	}

	removed, failures := New().RemoveGlobal(env, agents.ApplyOpts{})
	if len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	if removed == 0 {
		t.Fatal("expected RemoveGlobal to remove at least one artifact")
	}

	if _, err := os.Stat(PluginPath(env.Home)); err == nil {
		t.Error("the bridge plugin survived removal")
	}
	skills, err := Skills()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range skills {
		if _, err := os.Stat(filepath.Join(globalSkillsDir(env.Home), s.ID)); err == nil {
			t.Errorf("curated skill %s survived removal", s.ID)
		}
	}
	commands, err := Commands()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range commands {
		if _, err := os.Stat(filepath.Join(globalCommandsDir(env.Home), c.ID+commandFileExt)); err == nil {
			t.Errorf("curated command %s survived removal", c.ID)
		}
	}

	// The user's own skill in the shared root is byte-identical.
	skillAfter, err := os.ReadFile(userSkill)
	if err != nil {
		t.Fatalf("the user's skill was deleted: %v", err)
	}
	if string(skillAfter) != string(skillBefore) {
		t.Errorf("the user's skill changed:\n%s", skillAfter)
	}

	// The global config keeps everything except our own server entry. It
	// is re-marshalled on the way through, so compare the parsed shape
	// rather than the bytes.
	got, err := os.ReadFile(filepath.Join(globalConfigDir(env.Home), "opencode.json"))
	if err != nil {
		t.Fatalf("the user's global config was deleted: %v", err)
	}
	var root map[string]any
	if err := json.Unmarshal(got, &root); err != nil {
		t.Fatalf("global config is no longer valid JSON: %v\n%s", err, got)
	}
	section, ok := root["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("the user's mcp section was removed along with ours:\n%s", got)
	}
	if _, exists := section["gortex"]; exists {
		t.Errorf("the gortex server entry survived removal:\n%s", got)
	}
	if _, exists := section["other"]; !exists {
		t.Errorf("the user's own server entry was removed:\n%s", got)
	}
	if root["$schema"] != "https://opencode.ai/config.json" {
		t.Errorf("the user's $schema was lost:\n%s", got)
	}

	if got := GlobalArtifacts(env.Home); len(got) != 0 {
		t.Errorf("GlobalArtifacts should be empty after RemoveGlobal, got %v", got)
	}
}

// TestOpenCodeRemoveGlobalKeepsAForeignPlugin: the file name is not the
// evidence. A plugin somebody else wrote to plugin/gortex.js carries no
// PluginMarker and is not ours to delete.
func TestOpenCodeRemoveGlobalKeepsAForeignPlugin(t *testing.T) {
	env, _ := globalEnv(t)
	const foreign = "export const Plugin = async () => ({});\n"
	writeOpenCodeFile(t, PluginPath(env.Home), foreign)

	for _, path := range GlobalArtifacts(env.Home) {
		if path == PluginPath(env.Home) {
			t.Error("the preview promises to delete a plugin that is not ours")
		}
	}
	if _, failures := New().RemoveGlobal(env, agents.ApplyOpts{}); len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	got, err := os.ReadFile(PluginPath(env.Home))
	if err != nil {
		t.Fatalf("a foreign plugin was deleted: %v", err)
	}
	if string(got) != foreign {
		t.Errorf("a foreign plugin changed:\n%s", got)
	}
}

// TestOpenCodeRemoveGlobalKeepsACustomisedSkill pins the never-clobber
// posture writeCuratedFile takes on the way in: a Gortex skill the user
// edited is their work now. The wizard preview must agree — a path
// removal will keep may never appear in GlobalArtifacts.
func TestOpenCodeRemoveGlobalKeepsACustomisedSkill(t *testing.T) {
	env, _ := installedOpenCodeGlobal(t)

	skills, err := Skills()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) == 0 {
		t.Fatal("curated pack is empty")
	}
	customised := filepath.Join(globalSkillsDir(env.Home), skills[0].ID, skillFileName)
	body := renderSkill(skills[0]) + "\n<!-- my own note -->\n"
	writeOpenCodeFile(t, customised, body)

	for _, path := range GlobalArtifacts(env.Home) {
		if path == customised {
			t.Errorf("the preview promises to delete a customised skill: %s", path)
		}
	}

	if _, failures := New().RemoveGlobal(env, agents.ApplyOpts{}); len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	got, err := os.ReadFile(customised)
	if err != nil {
		t.Fatalf("the customised skill was deleted: %v", err)
	}
	if string(got) != body {
		t.Errorf("the customised skill changed:\n%s", got)
	}
}

// TestOpenCodeRemoveGlobalDryRunTouchesNothing keeps the preview honest in
// the other direction: it must count the same artifacts without writing.
func TestOpenCodeRemoveGlobalDryRunTouchesNothing(t *testing.T) {
	env, _ := installedOpenCodeGlobal(t)

	removed, failures := New().RemoveGlobal(env, agents.ApplyOpts{DryRun: true})
	if len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	if removed == 0 {
		t.Fatal("a dry run should still count what it would remove")
	}
	if _, err := os.Stat(PluginPath(env.Home)); err != nil {
		t.Error("dry run deleted the bridge plugin")
	}
	if got := GlobalArtifacts(env.Home); len(got) == 0 {
		t.Error("dry run removed the footprint")
	}
}

// TestOpenCodeRemoveGlobalNeedsAHome refuses rather than guessing at a root.
func TestOpenCodeRemoveGlobalNeedsAHome(t *testing.T) {
	removed, failures := New().RemoveGlobal(agents.Env{}, agents.ApplyOpts{})
	if removed != 0 || len(failures) != 1 {
		t.Fatalf("want (0, one failure), got (%d, %v)", removed, failures)
	}
}
