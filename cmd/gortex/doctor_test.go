package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// writeMCPConfig writes a {"mcpServers":{<name>: entry}} file and returns its path.
func writeMCPConfig(t *testing.T, name string, entry any) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp.json")
	body, err := json.Marshal(map[string]any{"mcpServers": map[string]any{name: entry}})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestDoctorVerifiesPathDaemonAndStaleStanza proves the F10 additions: the
// environment preamble probes the binary + daemon with consistent verdicts,
// and the MCP-stanza check classifies a Gortex-authored entry as current vs
// stale (and ignores non-Gortex / non-MCP files) so doctor can point at the
// `gortex install` migration instead of leaving a broken wiring silent.
func TestDoctorVerifiesPathDaemonAndStaleStanza(t *testing.T) {
	t.Run("environment_probe_is_consistent", func(t *testing.T) {
		env := doctorEnvironment()
		if env.DaemonSocket == "" {
			t.Error("environment must report the daemon socket path")
		}
		// Exactly one of {on-path, error} and {running, error} is set —
		// the verdict is internally consistent regardless of whether a
		// daemon or a PATH binary actually exists in this test env.
		if env.BinaryOnPath == (env.BinaryError != "") {
			t.Errorf("binary verdict inconsistent: onPath=%v err=%q", env.BinaryOnPath, env.BinaryError)
		}
		if env.DaemonRunning == (env.DaemonError != "") {
			t.Errorf("daemon verdict inconsistent: running=%v err=%q", env.DaemonRunning, env.DaemonError)
		}
	})

	t.Run("current_stanza", func(t *testing.T) {
		path := writeMCPConfig(t, "gortex", agents.DefaultGortexMCPEntry())
		st, ok := inspectMCPStanza(path)
		if !ok || st != "current" {
			t.Errorf("canonical stanza = (%q,%v), want (current,true)", st, ok)
		}
	})

	t.Run("stale_authored_stanza", func(t *testing.T) {
		// Gortex-authored (command=gortex, args[0]=mcp) but drifted shape.
		stale := map[string]any{
			"command": "gortex",
			"args":    []any{"mcp", "--legacy-flag"},
			"env":     map[string]any{},
		}
		path := writeMCPConfig(t, "gortex", stale)
		st, ok := inspectMCPStanza(path)
		if !ok || st != "stale" {
			t.Errorf("drifted authored stanza = (%q,%v), want (stale,true)", st, ok)
		}
	})

	t.Run("stale_under_nonstandard_key", func(t *testing.T) {
		// A Gortex-authored entry keyed by a non-"gortex" name is still found.
		stale := map[string]any{"command": "gortex", "args": []any{"mcp", "--x"}}
		path := writeMCPConfig(t, "gortex-daemon", stale)
		st, ok := inspectMCPStanza(path)
		if !ok || st != "stale" {
			t.Errorf("authored stanza under alt key = (%q,%v), want (stale,true)", st, ok)
		}
	})

	t.Run("non_gortex_entry_ignored", func(t *testing.T) {
		other := map[string]any{"command": "node", "args": []any{"server.js"}}
		path := writeMCPConfig(t, "other", other)
		if st, ok := inspectMCPStanza(path); ok {
			t.Errorf("a non-Gortex entry must be ignored; got (%q,%v)", st, ok)
		}
	})

	t.Run("non_mcp_file_ignored", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "notes.txt")
		if err := os.WriteFile(path, []byte("not json at all"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		if st, ok := inspectMCPStanza(path); ok {
			t.Errorf("a non-JSON file must be ignored; got (%q,%v)", st, ok)
		}
	})
}
