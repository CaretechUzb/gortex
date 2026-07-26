package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// TestDoctorCollapsesUninstalledAdapters: the adapter section describes the
// machine the user has. An agent that is neither installed nor holding Gortex
// files contributes only planned paths for writes that will never happen —
// and one such agent can contribute twenty, burying the lines that matter.
func TestDoctorCollapsesUninstalledAdapters(t *testing.T) {
	reports := []DoctorAgentReport{
		{Name: "claude-code", Detected: true, Configured: true, Files: []DoctorFileStatus{
			{Path: "/repo/.mcp.json", Status: "present", ByteSize: 12},
		}},
		{Name: "hermes", Detected: false, Configured: false, Files: []DoctorFileStatus{
			{Path: "/home/u/.hermes/skills/a/SKILL.md", Status: "missing", Planned: "would-create"},
			{Path: "/home/u/.hermes/skills/b/SKILL.md", Status: "missing", Planned: "would-create"},
		}},
		{Name: "zed", Detected: false, Configured: false},
	}

	t.Run("collapsed by default", func(t *testing.T) {
		doctorAll = false
		var buf bytes.Buffer
		printDoctorHuman(&buf, reports)
		got := buf.String()

		if strings.Contains(got, "SKILL.md") {
			t.Errorf("planned files for an uninstalled agent should be hidden:\n%s", got)
		}
		if !strings.Contains(got, "not installed (2): hermes, zed") {
			t.Errorf("absent agents should still be named:\n%s", got)
		}
		if !strings.Contains(got, "/repo/.mcp.json") {
			t.Errorf("detected agents keep their detail:\n%s", got)
		}
	})

	t.Run("--all restores the listing", func(t *testing.T) {
		doctorAll = true
		t.Cleanup(func() { doctorAll = false })
		var buf bytes.Buffer
		printDoctorHuman(&buf, reports)
		if !strings.Contains(buf.String(), "SKILL.md") {
			t.Errorf("--all should list everything:\n%s", buf.String())
		}
	})
}

// TestDoctorKeepsLeftoverConfig: an agent that is gone but still holds Gortex
// files is not noise — it is stale config the user probably wants to clean up,
// so it survives the collapse and says why it is there.
func TestDoctorKeepsLeftoverConfig(t *testing.T) {
	doctorAll = false
	var buf bytes.Buffer
	printDoctorHuman(&buf, []DoctorAgentReport{
		{Name: "windsurf", Detected: false, Configured: true, Files: []DoctorFileStatus{
			{Path: "/home/u/.codeium/mcp_config.json", Status: "present", ByteSize: 40},
		}},
	})
	got := buf.String()

	if strings.Contains(got, "not installed (1)") {
		t.Errorf("an agent holding Gortex files must not be collapsed away:\n%s", got)
	}
	if !strings.Contains(got, "leftover config") {
		t.Errorf("say why an uninstalled agent is still listed:\n%s", got)
	}
	if !strings.Contains(got, "/home/u/.codeium/mcp_config.json") {
		t.Errorf("the leftover file itself is the point:\n%s", got)
	}
}

// TestDoctorReservesTheCrossForRealGaps: an absent repo-local file is not a
// fault. A machine configured by `gortex install` needs no repo-local config
// at all, so marking it ✗ read as an accusation and implied work the user
// does not have to do. ✗ belongs to things that are broken or outdated.
func TestDoctorReservesTheCrossForRealGaps(t *testing.T) {
	doctorAll = false
	var buf bytes.Buffer
	printDoctorHuman(&buf, []DoctorAgentReport{{
		Name: "claude-code", Detected: true, Configured: true,
		Files: []DoctorFileStatus{
			{Path: "/repo/.mcp.json", Status: "missing", Planned: "would-create"},
			{Path: "/repo/.claude/settings.local.json", Status: "present", ByteSize: 26774},
			{Path: "/home/u/.gemini/mcp_config.json", Status: "present", ByteSize: 223, StanzaStatus: "stale"},
			{Path: "/repo/.broken.json", Status: "unreadable"},
		},
	}})
	got := buf.String()

	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, ".mcp.json") && !strings.Contains(line, "gemini") {
			if strings.Contains(line, glyphCross) {
				t.Errorf("an optional absent file must not be marked as a gap: %q", line)
			}
			if !strings.Contains(line, glyphAbsent) || !strings.Contains(line, "optional") {
				t.Errorf("an absent optional file should read as optional: %q", line)
			}
		}
		if strings.Contains(line, "settings.local.json") && !strings.Contains(line, glyphCheck) {
			t.Errorf("a present file is fine: %q", line)
		}
		// Present-but-outdated and unreadable are the real gaps.
		if strings.Contains(line, "gemini") && !strings.Contains(line, glyphWarn) {
			t.Errorf("an outdated stanza needs attention: %q", line)
		}
		if strings.Contains(line, ".broken.json") && !strings.Contains(line, glyphWarn) {
			t.Errorf("an unreadable file needs attention: %q", line)
		}
	}
	if !strings.Contains(got, "needs attention") {
		t.Errorf("the legend should explain the markers:\n%s", got)
	}
}
