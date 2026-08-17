package copilotcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

const userCopilotProse = "# House rules\n\nBe terse.\n"

func writeCopilotFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// userHookEntry is a hook a user added to OUR file. The CLI merges every
// hooks/*.json, so somebody who appended to gortex.json owns that entry —
// which is why removal strips rather than deletes.
func userHookEntry() map[string]any {
	return map[string]any{
		"type":       "command",
		"bash":       "my-own-linter",
		"powershell": "my-own-linter",
		"cwd":        ".",
	}
}

// installedCopilotGlobal runs a ModeGlobal Apply over a home that already
// holds the user's own MCP server, instructions prose, hook entry and
// skill, and returns the env plus the user skill's path. Starting from a
// real install rather than a hand-built fixture is what keeps the remover
// from asserting against a shape the writer no longer produces.
func installedCopilotGlobal(t *testing.T) (agents.Env, string) {
	t.Helper()
	env, _ := globalEnv(t)
	env.InstallGlobalInstructions = true

	writeCopilotFile(t, MCPConfigPath(env), `{"mcpServers":{"other":{"command":"node","args":["server.js"],"type":"local"}}}`)
	writeCopilotFile(t, GlobalInstructionsPath(env), userCopilotProse)

	// A hand-written skill in the SHARED ~/.copilot/skills root — the whole
	// reason removal cannot just delete the directory.
	userSkill := filepath.Join(copilotConfigHome(env), skillsDirName, "team-runbook", skillFileName)
	writeCopilotFile(t, userSkill, "---\nname: team-runbook\n---\n\nOurs, not Gortex's.\n")

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Appended after Apply so it survives into RemoveGlobal's input as a
	// genuine "the user edited our file" case.
	addUserHookEntry(t, env)

	if got := GlobalArtifacts(env.Home); len(got) == 0 {
		t.Fatal("expected a non-empty footprint after install")
	}
	return env, userSkill
}

func addUserHookEntry(t *testing.T, env agents.Env) {
	t.Helper()
	root := readJSON(t, HookConfigPath(env))
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("no hooks object in %s", HookConfigPath(env))
	}
	event := copilotHookEventNames["PreToolUse"]
	entries, _ := copilotHookList(hooks[event])
	hooks[event] = append(entries, userHookEntry())
	writeJSON(t, HookConfigPath(env), root)
}

// TestCopilotRemoveGlobalLeavesNoGortexArtifact is the round-trip
// contract: after Apply then RemoveGlobal nothing of Gortex's is left,
// and every user-owned thing that shared a file or a directory with it
// survives.
func TestCopilotRemoveGlobalLeavesNoGortexArtifact(t *testing.T) {
	env, userSkill := installedCopilotGlobal(t)
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

	// mcp-config.json: gortex gone, the user's server byte-for-byte intact.
	mcp := readJSON(t, MCPConfigPath(env))
	servers, _ := mcp["mcpServers"].(map[string]any)
	if _, exists := servers[serverName]; exists {
		t.Error("mcp-config.json still registers the gortex server")
	}
	wantOther := map[string]any{"command": "node", "args": []any{"server.js"}, "type": "local"}
	if got := servers["other"]; !reflect.DeepEqual(got, wantOther) {
		t.Errorf("the user's MCP server changed: %v", got)
	}

	// copilot-instructions.md: prose kept, marker block gone.
	md := readString(t, GlobalInstructionsPath(env))
	if strings.Contains(md, agents.GlobalRulesStartMarker) {
		t.Errorf("instructions still carry the gortex marker:\n%s", md)
	}
	if !strings.Contains(md, "Be terse.") {
		t.Errorf("instructions lost the user's prose:\n%s", md)
	}

	// gortex.json survives because the user's entry is still in it, and
	// that entry is unchanged.
	hookRoot := readJSON(t, HookConfigPath(env))
	if strings.Contains(readString(t, HookConfigPath(env)), "--agent="+hookAgentFlag) {
		t.Error("the hook config still invokes the gortex hook")
	}
	hooks, _ := hookRoot["hooks"].(map[string]any)
	entries, ok := copilotHookList(hooks[copilotHookEventNames["PreToolUse"]])
	if !ok || len(entries) != 1 {
		t.Fatalf("preToolUse should hold exactly the user's entry, got %v", hooks)
	}
	if !reflect.DeepEqual(entries[0], any(userHookEntry())) {
		t.Errorf("the user's hook entry changed: %v", entries[0])
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse"} {
		if left, _ := copilotHookList(hooks[copilotHookEventNames[event]]); len(left) != 0 {
			t.Errorf("%s should be empty after removal, got %v", event, left)
		}
	}

	// Curated skills and sub-agents are gone.
	for _, id := range CuratedSkillNames() {
		if _, err := os.Stat(curatedSkillPath(env, id)); err == nil {
			t.Errorf("curated skill %s survived removal", id)
		}
	}
	for _, id := range SubAgentNames() {
		if _, err := os.Stat(subAgentPath(env, id)); err == nil {
			t.Errorf("sub-agent %s survived removal", id)
		}
	}

	// The user's own skill in that same root is byte-identical.
	skillAfter, err := os.ReadFile(userSkill)
	if err != nil {
		t.Fatalf("the user's skill was deleted: %v", err)
	}
	if string(skillAfter) != string(skillBefore) {
		t.Errorf("the user's skill changed:\n%s", skillAfter)
	}

	if got := GlobalArtifacts(env.Home); len(got) != 0 {
		t.Errorf("GlobalArtifacts should be empty after RemoveGlobal, got %v", got)
	}
}

// TestCopilotRemoveGlobalDeletesAPureGortexHookFile is the other half of
// the hook policy: with nothing of the user's left in it, gortex.json is
// scaffolding we wrote and wholly ours to delete.
func TestCopilotRemoveGlobalDeletesAPureGortexHookFile(t *testing.T) {
	env, _ := globalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(HookConfigPath(env)); err != nil {
		t.Fatalf("apply wrote no hook config: %v", err)
	}

	if _, failures := New().RemoveGlobal(env, agents.ApplyOpts{}); len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	if _, err := os.Stat(HookConfigPath(env)); err == nil {
		t.Error("a hook file holding only gortex entries should be deleted")
	}
}

// TestCopilotRemoveGlobalKeepsACustomisedSkill pins the never-clobber
// posture Apply takes on the way in: a Gortex skill the user edited is
// their work now. The wizard preview must agree — a path removal will
// keep may never appear in GlobalArtifacts.
func TestCopilotRemoveGlobalKeepsACustomisedSkill(t *testing.T) {
	env, _ := installedCopilotGlobal(t)

	names := CuratedSkillNames()
	if len(names) == 0 {
		t.Fatal("curated pack is empty")
	}
	customised := curatedSkillPath(env, names[0])
	body := CuratedSkills()[names[0]] + "\n<!-- my own note -->\n"
	writeCopilotFile(t, customised, body)

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

// TestCopilotRemoveGlobalDryRunTouchesNothing keeps the preview honest in
// the other direction: it must count the same artifacts without writing.
func TestCopilotRemoveGlobalDryRunTouchesNothing(t *testing.T) {
	env, _ := installedCopilotGlobal(t)
	before := readString(t, HookConfigPath(env))

	removed, failures := New().RemoveGlobal(env, agents.ApplyOpts{DryRun: true})
	if len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	if removed == 0 {
		t.Fatal("a dry run should still count what it would remove")
	}
	if got := readString(t, HookConfigPath(env)); got != before {
		t.Error("dry run rewrote the hook config")
	}
	if got := GlobalArtifacts(env.Home); len(got) == 0 {
		t.Error("dry run removed the footprint")
	}
}

// TestCopilotRemoveGlobalNeedsAHome refuses rather than guessing at a root.
func TestCopilotRemoveGlobalNeedsAHome(t *testing.T) {
	t.Setenv(copilotHomeEnvVar, "")
	removed, failures := New().RemoveGlobal(agents.Env{}, agents.ApplyOpts{})
	if removed != 0 || len(failures) != 1 {
		t.Fatalf("want (0, one failure), got (%d, %v)", removed, failures)
	}
}

func readString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal([]byte(readString(t, path)), &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return root
}

func writeJSON(t *testing.T, path string, root map[string]any) {
	t.Helper()
	body, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	writeCopilotFile(t, path, string(body))
}
