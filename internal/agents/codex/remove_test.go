package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// userCodexConfig seeds ~/.codex/config.toml with content that is
// unambiguously the user's: a second MCP server and a hook that has
// nothing to do with Gortex. Everything RemoveGlobal does has to leave
// both exactly where they are.
const userCodexConfig = `[mcp_servers.other]
command = "node"
args = ["server.js"]

[[hooks.PreToolUse]]
matcher = "^Bash$"

[[hooks.PreToolUse.hooks]]
type = "command"
command = "my-own-linter"
`

const userCodexProse = "# My notes\n\nBe terse.\n"

func writeCodexFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// installedCodexGlobal runs a ModeGlobal Apply over a home already
// carrying the user's own config, prose and skill, and returns the env
// plus the user skill's path. Every RemoveGlobal test starts from a real
// install rather than a hand-built fixture, so a change to what Apply
// writes cannot leave the remover asserting against a shape that no
// longer ships.
func installedCodexGlobal(t *testing.T) (agents.Env, string) {
	t.Helper()
	env := codexGlobalEnv(t)
	env.InstallGlobalInstructions = true

	writeCodexFile(t, filepath.Join(env.Home, ".codex", "config.toml"), userCodexConfig)
	writeCodexFile(t, GlobalInstructionsPath(env.Home), userCodexProse)

	// A hand-written skill in the SHARED `.agents/skills` root — the whole
	// reason removal cannot just delete the directory.
	userSkill := filepath.Join(codexSkillsRoot(env.Home), "team-runbook", "SKILL.md")
	writeCodexFile(t, userSkill, "---\nname: team-runbook\n---\n\nOurs, not Gortex's.\n")

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := GlobalArtifacts(env.Home); len(got) == 0 {
		t.Fatal("expected a non-empty footprint after install")
	}
	return env, userSkill
}

// TestCodexRemoveGlobalLeavesNoGortexArtifact is the round-trip contract:
// after Apply then RemoveGlobal nothing of Gortex's is left, and every
// user-owned thing that shared a file or a directory with it survives.
func TestCodexRemoveGlobalLeavesNoGortexArtifact(t *testing.T) {
	env, userSkill := installedCodexGlobal(t)
	before, err := os.ReadFile(userSkill)
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

	root := readCodexConfig(t, env)

	// The gortex MCP stanza is gone; the user's other server is not.
	servers, _ := root["mcp_servers"].(map[string]any)
	if _, exists := servers["gortex"]; exists {
		t.Error("config.toml still registers the gortex MCP server")
	}
	if _, exists := servers["other"]; !exists {
		t.Error("config.toml lost the user's own MCP server")
	}

	// The direct-namespace opt-in is gone.
	if strings.Contains(rawCodexConfig(t, env), codexGortexToolNamespace) {
		t.Error("config.toml still opts gortex into direct tool namespaces")
	}

	// Every Gortex hook is gone; the user's PreToolUse hook is intact.
	if got := rawCodexConfig(t, env); strings.Contains(got, "--agent=codex") {
		t.Errorf("config.toml still carries a gortex hook:\n%s", got)
	}
	hooks, _ := root["hooks"].(map[string]any)
	entries, ok := codexHookList(hooks["PreToolUse"])
	if !ok || len(entries) != 1 {
		t.Fatalf("PreToolUse should hold exactly the user's entry, got %v", hooks["PreToolUse"])
	}
	if got := codexHookEntryCommand(entries[0]); got != "my-own-linter" {
		t.Errorf("the surviving PreToolUse entry is %q, want the user's linter", got)
	}
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "PostToolUse"} {
		if left, _ := codexHookList(hooks[event]); len(left) != 0 {
			t.Errorf("%s should be empty after removal, got %v", event, left)
		}
	}

	// AGENTS.md keeps the user's prose and loses the marker block.
	md, err := os.ReadFile(GlobalInstructionsPath(env.Home))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(md), agents.GlobalRulesStartMarker) {
		t.Errorf("AGENTS.md still carries the gortex marker:\n%s", md)
	}
	if !strings.Contains(string(md), "Be terse.") {
		t.Errorf("AGENTS.md lost the user's prose:\n%s", md)
	}

	// Every curated skill is gone from the shared root.
	skills, err := Skills()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range skills {
		if _, err := os.Stat(filepath.Join(codexSkillsRoot(env.Home), s.ID)); err == nil {
			t.Errorf("curated skill %s survived removal", s.ID)
		}
	}

	// The user's own skill in that same root is byte-identical.
	after, err := os.ReadFile(userSkill)
	if err != nil {
		t.Fatalf("the user's skill was deleted: %v", err)
	}
	if string(after) != string(before) {
		t.Errorf("the user's skill changed:\n%s", after)
	}

	if got := GlobalArtifacts(env.Home); len(got) != 0 {
		t.Errorf("GlobalArtifacts should be empty after RemoveGlobal, got %v", got)
	}
}

// TestCodexRemoveGlobalKeepsACustomisedSkill pins the posture
// SyncGlobalSkills established on the install side: a Gortex skill the
// user edited is their work now. Deleting it to satisfy a cleanup command
// is not a trade worth making, and the wizard preview must agree — a path
// removal will keep may never appear in GlobalArtifacts.
func TestCodexRemoveGlobalKeepsACustomisedSkill(t *testing.T) {
	env, _ := installedCodexGlobal(t)

	skills, err := Skills()
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) == 0 {
		t.Fatal("curated pack is empty")
	}
	customised := filepath.Join(codexSkillsRoot(env.Home), skills[0].ID, skillFileName)
	body := renderSkill(skills[0]) + "\n<!-- my own note -->\n"
	writeCodexFile(t, customised, body)

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

// TestCodexRemoveGlobalDryRunTouchesNothing keeps the preview honest in
// the other direction: it must count the same artifacts without writing.
func TestCodexRemoveGlobalDryRunTouchesNothing(t *testing.T) {
	env, _ := installedCodexGlobal(t)
	before := rawCodexConfig(t, env)

	removed, failures := New().RemoveGlobal(env, agents.ApplyOpts{DryRun: true})
	if len(failures) != 0 {
		t.Fatalf("RemoveGlobal failures: %v", failures)
	}
	if removed == 0 {
		t.Fatal("a dry run should still count what it would remove")
	}
	if got := rawCodexConfig(t, env); got != before {
		t.Error("dry run rewrote config.toml")
	}
	if got := GlobalArtifacts(env.Home); len(got) == 0 {
		t.Error("dry run removed the footprint")
	}
}

// TestCodexRemoveGlobalNeedsAHome refuses rather than guessing at a root.
func TestCodexRemoveGlobalNeedsAHome(t *testing.T) {
	removed, failures := New().RemoveGlobal(agents.Env{}, agents.ApplyOpts{})
	if removed != 0 || len(failures) != 1 {
		t.Fatalf("want (0, one failure), got (%d, %v)", removed, failures)
	}
}

func rawCodexConfig(t *testing.T, env agents.Env) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(env.Home, ".codex", "config.toml"))
	if err != nil {
		t.Fatalf("read config.toml: %v", err)
	}
	return string(data)
}
