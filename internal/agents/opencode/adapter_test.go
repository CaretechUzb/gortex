package opencode

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestOpenCodeUsesMCPSectionKey verifies we write under "mcp",
// not "mcpServers" — OpenCode's schema differs from the canonical
// Claude / Cursor shape — and that the config lands in a root
// `opencode.json`, the file OpenCode actually reads (not the legacy
// `.opencode/config.json`).
func TestOpenCodeUsesMCPSectionKey(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	// `.opencode/` is OpenCode's detection sentinel (it holds agents,
	// commands, skills) — present, but the MCP config does not live in it.
	if err := os.MkdirAll(filepath.Join(env.Root, ".opencode"), 0o755); err != nil {
		t.Fatal(err)
	}
	a := New()

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Three creates: opencode.json for MCP, AGENTS.md for the
	// instructions block OpenCode reads on every task, and the one
	// generated community skill NewEnv seeds.
	agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{agents.ActionCreate: 3})

	// The MCP config must be the root opencode.json, not the ignored
	// .opencode/config.json.
	if _, err := os.Stat(filepath.Join(env.Root, ".opencode", "config.json")); err == nil {
		t.Fatalf("must not write the legacy .opencode/config.json (OpenCode ignores it)")
	}
	cfg := agentstest.ReadJSON(t, filepath.Join(env.Root, "opencode.json"))
	if _, ok := cfg["mcpServers"]; ok {
		t.Fatalf("should not write mcpServers (that's Claude/Cursor shape): %v", cfg)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'mcp' section: %v", cfg)
	}
	gortex, ok := mcp["gortex"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'mcp.gortex': %v", mcp)
	}
	// command must be an array (OpenCode-specific)
	if _, ok := gortex["command"].([]any); !ok {
		t.Fatalf("command should be an array: %v", gortex)
	}
	if cfg["$schema"] != SchemaURL {
		t.Fatalf("expected $schema=%q, got %v", SchemaURL, cfg["$schema"])
	}

	agentstest.AssertIdempotent(t, a, env)
}

// TestOpenCodeMergesIntoExistingJSON verifies that when a project
// already has an opencode.json, we merge into it without clobbering
// the user's own keys, rather than creating a competing file.
func TestOpenCodeMergesIntoExistingJSON(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	cfgPath := filepath.Join(env.Root, "opencode.json")
	agentstest.WriteJSON(t, cfgPath, map[string]any{
		"$schema": SchemaURL,
		"theme":   "tokyonight",
		"mcp": map[string]any{
			"other": map[string]any{
				"type":    "local",
				"command": []string{"other-server"},
				"enabled": true,
			},
		},
	})
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	cfg := agentstest.ReadJSON(t, cfgPath)
	if cfg["theme"] != "tokyonight" {
		t.Fatalf("merge clobbered user's top-level key: %v", cfg)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'mcp' section: %v", cfg)
	}
	if _, ok := mcp["other"]; !ok {
		t.Fatalf("merge clobbered existing 'other' server: %v", mcp)
	}
	if _, ok := mcp["gortex"]; !ok {
		t.Fatalf("merge didn't add 'gortex': %v", mcp)
	}

	agentstest.AssertIdempotent(t, a, env)
}

// TestOpenCodeMergesIntoExistingJSONC verifies that an existing
// opencode.jsonc — comments and trailing commas included — is the file
// we merge into, the user's data survives, and no competing
// opencode.json is created.
func TestOpenCodeMergesIntoExistingJSONC(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	jsoncPath := filepath.Join(env.Root, "opencode.jsonc")
	const jsonc = `{
  // OpenCode project config
  "$schema": "https://opencode.ai/config.json",
  "theme": "tokyonight", /* dark theme */
  "mcp": {
    "other": { "type": "local", "command": ["other-server"], "enabled": true },
  },
}`
	if err := os.WriteFile(jsoncPath, []byte(jsonc), 0o644); err != nil {
		t.Fatal(err)
	}
	a := New()

	if _, err := a.Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// We must have merged into the .jsonc, not created a sibling .json.
	if _, err := os.Stat(filepath.Join(env.Root, "opencode.json")); err == nil {
		t.Fatalf("must not create opencode.json when opencode.jsonc already exists")
	}
	// A commented config is valid JSONC, not malformed — no backup.
	if _, err := os.Stat(jsoncPath + ".bak"); err == nil {
		t.Fatalf("a valid commented .jsonc must not be backed up as malformed")
	}

	cfg := agentstest.ReadJSON(t, jsoncPath)
	if cfg["theme"] != "tokyonight" {
		t.Fatalf("merge dropped user's data keys: %v", cfg)
	}
	mcp, ok := cfg["mcp"].(map[string]any)
	if !ok {
		t.Fatalf("missing 'mcp' section: %v", cfg)
	}
	if _, ok := mcp["other"]; !ok {
		t.Fatalf("merge clobbered existing 'other' server: %v", mcp)
	}
	if _, ok := mcp["gortex"]; !ok {
		t.Fatalf("merge didn't add 'gortex': %v", mcp)
	}

	agentstest.AssertIdempotent(t, a, env)
}
