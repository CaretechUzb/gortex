package opencode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// global_mcp_test.go covers the failure this package shipped once and
// must never ship again.
//
// `gortex install` runs in ModeGlobal. It installs 21 skills, 20 slash
// commands and an enforcement plugin, every one of which tells the model
// to reach for the Gortex tools — and it used to register no MCP server
// at that scope, on the theory that `gortex init` would put one in each
// repo. The result on a real machine was an OpenCode that had been
// taught to ask for tools that were not mounted, and the only visible
// symptom was the agent reporting a Gortex integration failure.
//
// The invariant these tests hold: if global mode installs guidance, it
// installs the server that guidance depends on.

// TestGlobalInstallRegistersTheMCPServer is the direct regression.
func TestGlobalInstallRegistersTheMCPServer(t *testing.T) {
	env, _ := globalEnv(t)

	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	entry := gortexServerEntry(t, GlobalConfigPath(env.Home))
	if entry["type"] != "local" {
		t.Errorf("type = %v, want local", entry["type"])
	}
	if entry["enabled"] != true {
		t.Errorf("enabled = %v, want true", entry["enabled"])
	}
	command, ok := entry["command"].([]any)
	if !ok || len(command) != 2 || command[0] != "gortex" || command[1] != "mcp" {
		t.Errorf("command = %v, want [gortex mcp]", entry["command"])
	}
}

// TestGlobalInstallNeverShipsGuidanceWithoutTools is the invariant stated
// as a test, so a future change that adds another user-level teaching
// surface cannot reintroduce the same asymmetry by accident.
func TestGlobalInstallNeverShipsGuidanceWithoutTools(t *testing.T) {
	env, _ := globalEnv(t)

	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	guidance := map[string]string{
		"curated skills":     globalSkillsDir(env.Home),
		"slash commands":     globalCommandsDir(env.Home),
		"enforcement plugin": filepath.Dir(PluginPath(env.Home)),
	}
	installed := false
	for label, dir := range guidance {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) == 0 {
			continue
		}
		installed = true
		t.Logf("%s installed (%d entries)", label, len(entries))
	}
	if !installed {
		t.Skip("global mode installed no guidance surface; nothing to depend on a server")
	}

	// Guidance is present, so a server entry must be too.
	gortexServerEntry(t, GlobalConfigPath(env.Home))
}

// TestGlobalInstallPreservesAUserConfig pins the merge: we add one key to
// a file the user owns and touch nothing else in it.
func TestGlobalInstallPreservesAUserConfig(t *testing.T) {
	env, _ := globalEnv(t)
	writeOpenCodeFile(t, filepath.Join(globalConfigDir(env.Home), "opencode.json"), userGlobalOpenCodeConfig)

	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	root := readOpenCodeConfig(t, GlobalConfigPath(env.Home))
	section, ok := root["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("mcp section missing: %v", root)
	}
	if _, exists := section["gortex"]; !exists {
		t.Error("our server was not added")
	}
	if _, exists := section["other"]; !exists {
		t.Error("the user's own server was dropped")
	}
}

// TestGlobalConfigPathPrefersAnExistingJSONC: a user whose config is
// comment-bearing keeps that file. Writing the sibling .json instead
// would leave two configs and register the server in the one OpenCode
// does not read — the same invisible failure in a new costume.
func TestGlobalConfigPathPrefersAnExistingJSONC(t *testing.T) {
	home := t.TempDir()
	jsonc := filepath.Join(home, ".config", "opencode", "opencode.jsonc")
	writeOpenCodeFile(t, jsonc, "{\n  // mine\n  \"$schema\": \"https://opencode.ai/config.json\"\n}\n")

	if got := GlobalConfigPath(home); got != jsonc {
		t.Errorf("GlobalConfigPath = %q, want the existing %q", got, jsonc)
	}
}

// TestGlobalInstallMergesIntoAJSONCConfig is the end-to-end of the above:
// the comment-bearing file is the one that gains the server.
func TestGlobalInstallMergesIntoAJSONCConfig(t *testing.T) {
	env, _ := globalEnv(t)
	jsonc := filepath.Join(globalConfigDir(env.Home), "opencode.jsonc")
	writeOpenCodeFile(t, jsonc, "{\n  // hand written\n  \"$schema\": \"https://opencode.ai/config.json\"\n}\n")

	if _, err := New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	gortexServerEntry(t, jsonc)
	if _, err := os.Stat(filepath.Join(globalConfigDir(env.Home), "opencode.json")); err == nil {
		t.Error("wrote a sibling opencode.json; the .jsonc was the file OpenCode reads")
	}
}

// TestGlobalPlanNamesTheConfig keeps doctor and --print-config honest:
// both read Plan(), so a path Apply writes but Plan omits is invisible.
func TestGlobalPlanNamesTheConfig(t *testing.T) {
	env, _ := globalEnv(t)

	plan, err := New().Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	want := GlobalConfigPath(env.Home)
	for _, f := range plan.Files {
		if f.Path == want {
			return
		}
	}
	t.Errorf("Plan did not name %s; files=%v", want, plan.Files)
}

func gortexServerEntry(t *testing.T, path string) map[string]any {
	t.Helper()
	root := readOpenCodeConfig(t, path)
	section, ok := root["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("no mcp section in %s: %v", path, root)
	}
	entry, ok := section["gortex"].(map[string]any)
	if !ok {
		t.Fatalf("no gortex server registered in %s: %v", path, section)
	}
	return entry
}

func readOpenCodeConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var root map[string]any
	if err := json.Unmarshal(agents.StripJSONComments(data), &root); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return root
}
