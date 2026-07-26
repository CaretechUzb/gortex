package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeEffectivenessLog(t *testing.T, rows []hookEffectiveness) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hook-effectiveness.jsonl")
	var buf strings.Builder
	for _, row := range rows {
		blob, err := json.Marshal(row)
		if err != nil {
			t.Fatal(err)
		}
		buf.Write(blob)
		buf.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(buf.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GORTEX_HOOK_EFFECTIVENESS_LOG", path)
	return path
}

func stamp(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

func boolPtr(v bool) *bool { return &v }

func TestReadEffectivenessAggregatesWithinWindow(t *testing.T) {
	now := time.Now()
	writeEffectivenessLog(t, []hookEffectiveness{
		{Timestamp: stamp(now.Add(-30 * time.Minute)), Event: "SessionStart", EmittedContext: true, DaemonReachable: boolPtr(true), DurationMS: 40},
		{Timestamp: stamp(now.Add(-20 * time.Minute)), Event: "UserPromptSubmit", EmittedContext: false, DaemonReachable: boolPtr(true), DurationMS: 10},
		{Timestamp: stamp(now.Add(-10 * time.Minute)), Event: "UserPromptSubmit", EmittedContext: false, DaemonReachable: boolPtr(false), DurationMS: 12},
		// Outside the window: must not count toward runs, but must still
		// move LastSeen so a caller can say "last ran N days ago" instead of
		// only "not in window".
		{Timestamp: stamp(now.Add(-72 * time.Hour)), Event: "PostToolUse", EmittedContext: true, DaemonReachable: boolPtr(true)},
	})

	got, err := ReadEffectiveness(now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !got.Present {
		t.Fatal("log exists, Present should be true")
	}
	if got.TotalRows != 4 {
		t.Errorf("totalRows=%d want 4", got.TotalRows)
	}
	if got.WindowRows != 3 {
		t.Errorf("windowRows=%d want 3", got.WindowRows)
	}

	ups := got.Events["UserPromptSubmit"]
	if ups.Runs != 2 || ups.Emitted != 0 {
		t.Errorf("UserPromptSubmit runs=%d emitted=%d want 2/0", ups.Runs, ups.Emitted)
	}
	if ups.DaemonKnown != 2 || ups.DaemonUp != 1 {
		t.Errorf("daemon up=%d/%d want 1/2", ups.DaemonUp, ups.DaemonKnown)
	}
	if ups.TotalMS != 22 {
		t.Errorf("totalMS=%d want 22", ups.TotalMS)
	}
	if !got.Ran("SessionStart") {
		t.Error("SessionStart ran in the window")
	}
	if got.Ran("PostToolUse") {
		t.Error("PostToolUse is outside the window and must not count as ran")
	}
	if last := got.Events["PostToolUse"].LastSeen; last.IsZero() {
		t.Error("LastSeen must be tracked across the whole log, not just the window")
	}
}

// TestReadEffectivenessMissingLogIsNotAnError: no log means no hook has ever
// run, which is the answer doctor is looking for — not a failure.
func TestReadEffectivenessMissingLogIsNotAnError(t *testing.T) {
	t.Setenv("GORTEX_HOOK_EFFECTIVENESS_LOG", filepath.Join(t.TempDir(), "absent.jsonl"))

	got, err := ReadEffectiveness(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("missing log should not error: %v", err)
	}
	if got.Present || got.TotalRows != 0 || got.Ran("SessionStart") {
		t.Errorf("got %+v", got)
	}
}

// TestReadEffectivenessSkipsMalformedRows — the log is appended by
// short-lived processes, so a torn line during a crash is expected and must
// not blind the reader to the rows around it.
func TestReadEffectivenessSkipsMalformedRows(t *testing.T) {
	now := time.Now()
	path := writeEffectivenessLog(t, []hookEffectiveness{
		{Timestamp: stamp(now.Add(-time.Minute)), Event: "SessionStart", EmittedContext: true},
	})
	handle, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle.WriteString("garbage\n{\"ts\":\"not-a-time\",\"event\":\"SessionStart\"}\n{\"ts\":\"" + stamp(now) + "\",\"even"); err != nil {
		t.Fatal(err)
	}
	handle.Close()

	got, err := ReadEffectiveness(now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got.Events["SessionStart"].Runs != 1 {
		t.Errorf("runs=%d want 1 — the good row survives the garbage", got.Events["SessionStart"].Runs)
	}
}

// TestEventNamesSorted keeps the rendered table stable across runs.
func TestEventNamesSorted(t *testing.T) {
	now := time.Now()
	writeEffectivenessLog(t, []hookEffectiveness{
		{Timestamp: stamp(now), Event: "UserPromptSubmit"},
		{Timestamp: stamp(now), Event: "PostToolUse"},
		{Timestamp: stamp(now), Event: "SessionStart"},
	})

	got, err := ReadEffectiveness(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	names := got.EventNames()
	want := []string{"PostToolUse", "SessionStart", "UserPromptSubmit"}
	if len(names) != len(want) {
		t.Fatalf("names=%v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("names=%v want %v", names, want)
		}
	}
}

func TestSetAgentResolvesTheDefault(t *testing.T) {
	t.Cleanup(func() { SetAgent("") })
	cases := map[string]string{
		"":            AgentClaudeCode, // the --agent flag's documented default
		"claude":      AgentClaudeCode,
		"claude-code": AgentClaudeCode,
		"  Claude  ":  AgentClaudeCode,
		"codex":       "codex",
		"CODEX":       "codex",
		"kimi":        "kimi",
	}
	for in, want := range cases {
		SetAgent(in)
		if got := ActiveAgent(); got != want {
			t.Errorf("SetAgent(%q) → %q want %q", in, got, want)
		}
	}
}

func TestReadEffectivenessPartitionsByAgent(t *testing.T) {
	now := time.Now()
	writeEffectivenessLog(t, []hookEffectiveness{
		{Timestamp: stamp(now.Add(-10 * time.Minute)), Event: "SessionStart", Agent: "claude-code", EmittedContext: true},
		{Timestamp: stamp(now.Add(-9 * time.Minute)), Event: "PreToolUse", Agent: "claude-code", EmittedContext: true},
		{Timestamp: stamp(now.Add(-8 * time.Minute)), Event: "PreToolUse", Agent: "codex", EmittedContext: false},
	})

	got, err := ReadEffectiveness(now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Attributed {
		t.Fatal("rows carry an agent, so the summary is attributed")
	}
	if got.UnattributedRows != 0 {
		t.Errorf("unattributed=%d want 0", got.UnattributedRows)
	}
	if got.Events["PreToolUse"].Runs != 2 {
		t.Errorf("union PreToolUse=%d want 2", got.Events["PreToolUse"].Runs)
	}

	codex := got.ForAgent("codex")
	if codex["PreToolUse"].Runs != 1 {
		t.Errorf("codex PreToolUse=%d want 1", codex["PreToolUse"].Runs)
	}
	// The load-bearing assertion: Claude Code running SessionStart must not
	// vouch for Codex's SessionStart, which is the hook Codex skips.
	if codex["SessionStart"].Runs != 0 {
		t.Errorf("codex SessionStart=%d want 0 — another agent's rows leaked in", codex["SessionStart"].Runs)
	}
	if names := got.AgentNames(); len(names) != 2 || names[0] != "claude-code" || names[1] != "codex" {
		t.Errorf("AgentNames=%v want [claude-code codex]", names)
	}
}

// TestForAgentFallbackIsAsymmetric pins the rule that keeps an upgrade from
// inventing a blocker: a log that predates attribution cannot say a given
// agent was silent, so the union stands in. Once the log *is* attributed, an
// agent with no rows really was silent and must read as such.
func TestForAgentFallbackIsAsymmetric(t *testing.T) {
	now := time.Now()

	t.Run("unattributed log falls back to the union", func(t *testing.T) {
		writeEffectivenessLog(t, []hookEffectiveness{
			{Timestamp: stamp(now.Add(-time.Minute)), Event: "SessionStart", EmittedContext: true},
		})
		got, err := ReadEffectiveness(now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if got.Attributed {
			t.Fatal("no row carries an agent")
		}
		if got.UnattributedRows != 1 {
			t.Errorf("unattributed=%d want 1", got.UnattributedRows)
		}
		if got.ForAgent("codex")["SessionStart"].Runs != 1 {
			t.Error("an unattributed log must not be read as proof that codex was silent")
		}
	})

	t.Run("attributed log reports a silent agent as silent", func(t *testing.T) {
		writeEffectivenessLog(t, []hookEffectiveness{
			{Timestamp: stamp(now.Add(-time.Minute)), Event: "SessionStart", Agent: "claude-code", EmittedContext: true},
		})
		got, err := ReadEffectiveness(now.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(got.ForAgent("codex")) != 0 {
			t.Error("codex has no rows in an attributed log, which is real evidence of silence")
		}
	})
}

// TestLogRecordsActiveAgent closes the loop: the writer stamps what SetAgent
// was told, and the reader partitions on it.
func TestLogRecordsActiveAgent(t *testing.T) {
	t.Cleanup(func() { SetAgent("") })
	path := filepath.Join(t.TempDir(), "hook-effectiveness.jsonl")
	t.Setenv("GORTEX_HOOK_EFFECTIVENESS_LOG", path)

	SetAgent("codex")
	logHookEffectiveness("SessionStart", true, true, 0, 5*time.Millisecond)

	got, err := ReadEffectiveness(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Attributed {
		t.Fatal("the writer did not stamp the agent")
	}
	if got.ForAgent("codex")["SessionStart"].Runs != 1 {
		t.Errorf("codex rows=%+v", got.ByAgent)
	}
	if len(got.ForAgent(AgentClaudeCode)) != 0 {
		t.Error("the row belongs to codex only")
	}
}
