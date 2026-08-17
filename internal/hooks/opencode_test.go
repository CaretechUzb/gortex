package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

func TestHandleOpenCode_EnvelopeMapping(t *testing.T) {
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 7)
	stubTrackedScope(t, true)
	stubPromptProbe(t, []grepSymbolHit{
		{Name: "ValidateToken", Kind: "function", FilePath: "internal/auth/token.go", Line: 42},
	})
	withFakeStatus(t, func() (*daemon.StatusResponse, error) {
		return &daemon.StatusResponse{
			Version: "1.0.0", Ready: true,
			TrackedRepos: []daemon.TrackedRepoStatus{{Name: "repo", Path: "/tmp/repo", Workspace: "repo", Nodes: 10}},
			Workspaces:   []daemon.WorkspaceSummary{{Slug: "repo"}},
		}, nil
	})

	tests := []struct {
		name    string
		payload string
		check   func(t *testing.T, d BridgeDecision)
	}{
		{
			// tool.execute.before blocks by throwing, so a deny has to arrive
			// as Block+Reason for the plugin to raise.
			name:    "tool_call on indexed source blocks",
			payload: `{"event":"tool_call","tool_name":"read","tool_input":{"file_path":"internal/auth/token.go"},"cwd":"/tmp/repo"}`,
			check: func(t *testing.T, d BridgeDecision) {
				if !d.Block {
					t.Fatalf("indexed read was not blocked: %+v", d)
				}
				if !strings.Contains(d.Reason, `read(target:{symbol:`) {
					t.Fatalf("block reason must name the graph replacement: %q", d.Reason)
				}
			},
		},
		{
			// OpenCode's own hook names are dotted; a plugin forwarding them
			// verbatim must land on the same slot.
			name:    "dotted tool.execute.before is the same slot",
			payload: `{"event":"tool.execute.before","tool_name":"read","tool_input":{"file_path":"internal/auth/token.go"},"cwd":"/tmp/repo"}`,
			check: func(t *testing.T, d BridgeDecision) {
				if !d.Block {
					t.Fatalf("dotted event name was not routed: %+v", d)
				}
			},
		},
		{
			name:    "permission.ask denies the same call",
			payload: `{"event":"permission.ask","tool_name":"grep","tool_input":{"pattern":"ValidateToken"},"cwd":"/tmp/repo"}`,
			check: func(t *testing.T, d BridgeDecision) {
				if !d.Block || d.Reason == "" {
					t.Fatalf("permission.ask on indexed source must deny: %+v", d)
				}
			},
		},
		{
			name:    "session event returns orientation",
			payload: `{"event":"session","cwd":"/tmp/repo"}`,
			check: func(t *testing.T, d BridgeDecision) {
				if !strings.Contains(d.Orientation, "Gortex Session Orientation") {
					t.Fatalf("session event lost the orientation: %+v", d)
				}
				if d.Block {
					t.Error("a session event must never block")
				}
			},
		},
		{
			name:    "prompt event returns additional context",
			payload: `{"event":"chat.message","prompt":"fix the token validation bug","cwd":"/tmp/repo"}`,
			check: func(t *testing.T, d BridgeDecision) {
				if !strings.Contains(d.AdditionalContext, "ValidateToken") {
					t.Fatalf("prompt event lost the per-turn injection: %+v", d)
				}
				if d.Block {
					t.Error("a prompt event must never block")
				}
			},
		},
		{
			name:    "malformed JSON is an empty fail-open decision",
			payload: `{not json`,
			check: func(t *testing.T, d BridgeDecision) {
				if d != (BridgeDecision{}) {
					t.Fatalf("malformed JSON must fail open: %+v", d)
				}
			},
		},
		{
			name:    "unknown event is a no-op",
			payload: `{"event":"tool.execute.after","tool_name":"read","cwd":"/tmp/repo"}`,
			check: func(t *testing.T, d BridgeDecision) {
				if d != (BridgeDecision{}) {
					t.Fatalf("unmapped event must be a no-op: %+v", d)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.check(t, handleOpenCode([]byte(tt.payload), 0, ModeDeny))
		})
	}
}

// The bridge contract says the plugin normalizes tool names; the table is the
// safety net for a build that forwards OpenCode's own lowercase names, and it
// must be a no-op on already-normalized input.
func TestOpenCodeToolNameNormalisation(t *testing.T) {
	cases := map[string]string{
		"read": "Read", "write": "Write", "edit": "Edit", "bash": "Bash",
		"grep": "Grep", "glob": "Glob", "task": "Task",
		"Read": "Read", "webfetch": "webfetch", "todowrite": "todowrite",
	}
	for input, want := range cases {
		if got := openCodeToolName(input); got != want {
			t.Errorf("openCodeToolName(%q)=%q want %q", input, got, want)
		}
	}
}

func TestHandleOpenCode_StandsDownWhenDaemonUnreachable(t *testing.T) {
	withDaemonReachable(t, false)
	stubIndexedFile(t, true, 7)
	stubTrackedScope(t, true)
	stubPromptProbe(t, []grepSymbolHit{{Name: "ValidateToken", FilePath: "internal/auth/token.go", Line: 42}})

	for _, payload := range []string{
		`{"event":"tool_call","tool_name":"read","tool_input":{"file_path":"internal/auth/token.go"},"cwd":"/tmp/repo"}`,
		`{"event":"chat.message","prompt":"fix the token validation bug","cwd":"/tmp/repo"}`,
	} {
		if d := handleOpenCode([]byte(payload), 0, ModeDeny); d != (BridgeDecision{}) {
			t.Fatalf("outage must yield an empty decision for %s: %+v", payload, d)
		}
	}
}

func TestRunOpenCodeReadsStdinAndWritesDecision(t *testing.T) {
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 7)

	payload := []byte(`{"event":"tool_call","tool_name":"read","tool_input":{"file_path":"internal/auth/token.go"},"cwd":"/tmp/repo"}`)
	out := captureStdout(t, func() {
		withStdin(t, payload, func() { RunOpenCode(0, ModeDeny) })
	})
	var decision BridgeDecision
	if err := json.Unmarshal([]byte(out), &decision); err != nil {
		t.Fatalf("invalid bridge decision %q: %v", out, err)
	}
	if !decision.Block {
		t.Fatalf("RunOpenCode did not emit the block: %q", out)
	}
}

// The bridge core wrote no telemetry at all before both hosts shared it,
// which left a `gortex doctor` probe with no denominator — a working bridge
// was indistinguishable from one that never fires. Pi gets the fix for free.
func TestBridgeCoreLogsHookEffectivenessForBothHosts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "effectiveness.jsonl")
	t.Setenv("GORTEX_HOOK_EFFECTIVENESS_LOG", path)
	withDaemonReachable(t, true)
	stubIndexedFile(t, true, 7)

	toolCall := `{"event":"tool_call","tool_name":"read","tool_input":{"file_path":"internal/auth/token.go"},"cwd":"/tmp/repo"}`
	if d := handleOpenCode([]byte(toolCall), 0, ModeDeny); !d.Block {
		t.Fatalf("precondition: expected a block, got %+v", d)
	}
	if d := handlePi([]byte(`{"event":"tool_call","tool_name":"Read","tool_input":{"file_path":"internal/auth/token.go"},"cwd":"/tmp/repo"}`), 0, ModeDeny); !d.Block {
		t.Fatalf("precondition: expected a Pi block, got %+v", d)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the bridge core must leave hook-effectiveness records: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("records=%d want 2 (one per bridged host): %s", len(lines), data)
	}
	for _, line := range lines {
		var record hookEffectiveness
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("invalid effectiveness JSONL %q: %v", line, err)
		}
		if record.Event != "PreToolUse" || !hookEffectivenessEvents[record.Event] {
			t.Fatalf("a tool_call must be logged under the allowlisted PreToolUse: %+v", record)
		}
		if !record.EmittedContext {
			t.Fatalf("a block is an emitted decision: %+v", record)
		}
	}
}
