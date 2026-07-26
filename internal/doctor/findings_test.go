package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/zzet/gortex/internal/hooks"
)

func codexAgent() AgentHooks {
	return AgentHooks{
		Agent:             "codex",
		Configured:        map[string]int{"SessionStart": 1, "UserPromptSubmit": 1, "PreToolUse": 2, "PostToolUse": 1},
		RequiresTrust:     true,
		TrustRemedy:       "run `/hooks` inside Codex",
		InstructionsPath:  "/home/u/.codex/AGENTS.md",
		InstructionsWired: true,
	}
}

func activity(events map[string]hooks.EventActivity) hooks.EffectivenessSummary {
	if events == nil {
		events = map[string]hooks.EventActivity{}
	}
	return hooks.EffectivenessSummary{Present: true, Events: events}
}

func findingWith(t *testing.T, findings []Finding, substr string) Finding {
	t.Helper()
	for _, f := range findings {
		if strings.Contains(f.Summary, substr) {
			return f
		}
	}
	t.Fatalf("no finding containing %q in %+v", substr, findings)
	return Finding{}
}

func assertNoFindingWith(t *testing.T, findings []Finding, substr string) {
	t.Helper()
	for _, f := range findings {
		if strings.Contains(f.Summary, substr) {
			t.Fatalf("unexpected finding %q in %+v", substr, findings)
		}
	}
}

// TestConfiguredButNeverRanIsABlocker is the reported failure: every hook is
// on disk and none of them runs, because Codex skips hooks it has not been
// told to trust. Config alone cannot distinguish this from a healthy install,
// so the finding has to carry the remedy.
func TestConfiguredButNeverRanIsABlocker(t *testing.T) {
	findings := Diagnose(codexAgent(), activity(nil), Adoption{}, time.Now())

	got := findingWith(t, findings, "none has run")
	if got.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", got.Severity)
	}
	if !strings.Contains(got.Remedy, "/hooks") {
		t.Errorf("remedy should name /hooks, got %q", got.Remedy)
	}
	if !HasBlocker(findings) {
		t.Error("HasBlocker should be true so the command exits non-zero")
	}
	if findings[0].Severity != SeverityBlocker {
		t.Errorf("blockers must sort first, got %+v", findings)
	}
}

// TestNeverRanWithoutTrustGateAvoidsTheTrustRemedy keeps the Codex-specific
// explanation from being handed to agents that do not gate on trust — a
// wrong remedy costs more than a vague one.
func TestNeverRanWithoutTrustGateAvoidsTheTrustRemedy(t *testing.T) {
	agent := codexAgent()
	agent.Agent = "claude-code"
	agent.RequiresTrust = false
	agent.TrustRemedy = ""

	findings := Diagnose(agent, activity(nil), Adoption{}, time.Now())
	got := findingWith(t, findings, "none has run")
	if got.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", got.Severity)
	}
	if strings.Contains(got.Summary, "trusted") {
		t.Errorf("trust wording leaked to a non-gating agent: %q", got.Summary)
	}
}

// TestSingleStaleHookIsReported covers the upgrade case: trust is recorded
// per hook hash, so changing one hook's definition silences only that hook.
func TestSingleStaleHookIsReported(t *testing.T) {
	now := time.Now()
	findings := Diagnose(codexAgent(), activity(map[string]hooks.EventActivity{
		"SessionStart":     {Runs: 0, LastSeen: now.Add(-72 * time.Hour)},
		"UserPromptSubmit": {Runs: 5, Emitted: 5},
		"PreToolUse":       {Runs: 40, Emitted: 12},
		"PostToolUse":      {Runs: 3, Emitted: 1},
	}), Adoption{GortexCalls: 10, ShellCalls: 1}, now)

	got := findingWith(t, findings, "SessionStart is configured but never ran")
	if got.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", got.Severity)
	}
	if !strings.Contains(got.Summary, "3d ago") {
		t.Errorf("should date the last run so a stale hook is distinguishable: %q", got.Summary)
	}
	assertNoFindingWith(t, findings, "none has run")
}

// TestHookThatRunsButNeverInjectsIsAWarn separates "never fires" from "fires
// and has nothing to say" — the same symptom, entirely different causes.
func TestHookThatRunsButNeverInjectsIsAWarn(t *testing.T) {
	findings := Diagnose(codexAgent(), activity(map[string]hooks.EventActivity{
		"SessionStart":     {Runs: 4, Emitted: 4, DaemonKnown: 4, DaemonUp: 4},
		"UserPromptSubmit": {Runs: 9, Emitted: 0, DaemonKnown: 9, DaemonUp: 9},
		"PreToolUse":       {Runs: 9, Emitted: 3, DaemonKnown: 9, DaemonUp: 9},
		"PostToolUse":      {Runs: 9, Emitted: 3, DaemonKnown: 9, DaemonUp: 9},
	}), Adoption{GortexCalls: 20, ShellCalls: 2}, time.Now())

	got := findingWith(t, findings, "UserPromptSubmit ran 9")
	if got.Severity != SeverityWarn {
		t.Errorf("severity=%s want WARN", got.Severity)
	}
	if HasBlocker(findings) {
		t.Errorf("a silent-but-live hook is not a blocker: %+v", findings)
	}
}

// TestUnreachableDaemonOutranksSilence: a hook that cannot reach the daemon
// is why it is silent, so reporting both would bury the cause under a
// symptom.
func TestUnreachableDaemonOutranksSilence(t *testing.T) {
	findings := Diagnose(codexAgent(), activity(map[string]hooks.EventActivity{
		"SessionStart":     {Runs: 3, Emitted: 0, DaemonKnown: 3, DaemonUp: 0},
		"UserPromptSubmit": {Runs: 3, Emitted: 3, DaemonKnown: 3, DaemonUp: 3},
		"PreToolUse":       {Runs: 3, Emitted: 3, DaemonKnown: 3, DaemonUp: 3},
		"PostToolUse":      {Runs: 3, Emitted: 3, DaemonKnown: 3, DaemonUp: 3},
	}), Adoption{GortexCalls: 5, ShellCalls: 1}, time.Now())

	got := findingWith(t, findings, "never reached the daemon")
	if got.Severity != SeverityBlocker {
		t.Errorf("severity=%s want BLOCKER", got.Severity)
	}
	if !strings.Contains(got.Remedy, "daemon start") {
		t.Errorf("remedy should start the daemon, got %q", got.Remedy)
	}
	assertNoFindingWith(t, findings, "SessionStart ran 3 time(s) and injected context 0")
}

func TestInstructionsFindings(t *testing.T) {
	healthy := activity(map[string]hooks.EventActivity{"SessionStart": {Runs: 2, Emitted: 2}})

	t.Run("missing block", func(t *testing.T) {
		agent := codexAgent()
		agent.InstructionsWired = false
		got := findingWith(t, Diagnose(agent, healthy, Adoption{}, time.Now()), "No Gortex rule block")
		if got.Severity != SeverityWarn || got.Remedy != "gortex install" {
			t.Errorf("got %+v", got)
		}
	})

	t.Run("shadowed block", func(t *testing.T) {
		agent := codexAgent()
		agent.InstructionsShadowed = "/home/u/.codex/AGENTS.override.md"
		findings := Diagnose(agent, healthy, Adoption{}, time.Now())
		got := findingWith(t, findings, "instead of")
		if got.Severity != SeverityWarn {
			t.Errorf("severity=%s want WARN", got.Severity)
		}
		// A wired-but-shadowed block must not also be reported as missing:
		// the fix is different and the pair reads as contradictory.
		assertNoFindingWith(t, findings, "No Gortex rule block")
	})
}

func TestAdoptionFindings(t *testing.T) {
	healthy := activity(map[string]hooks.EventActivity{
		"SessionStart": {Runs: 1, Emitted: 1}, "UserPromptSubmit": {Runs: 1, Emitted: 1},
		"PreToolUse": {Runs: 1, Emitted: 1}, "PostToolUse": {Runs: 1, Emitted: 1},
	})

	cases := []struct {
		name     string
		adoption Adoption
		want     Severity
		substr   string
	}{
		{"all shell", Adoption{FilesFound: 3, InWindow: 3, ShellCalls: 20}, SeverityBlocker, "zero Gortex tool calls"},
		{"mostly shell", Adoption{FilesFound: 3, InWindow: 3, GortexCalls: 3, ShellCalls: 20, ShellReads: 15}, SeverityWarn, "13% of tool calls"},
		{"healthy", Adoption{FilesFound: 3, InWindow: 3, GortexCalls: 90, ShellCalls: 10}, SeverityOK, "90% of tool calls"},
		{"no transcripts", Adoption{}, SeverityInfo, "could not be measured"},
		{"no tool calls", Adoption{FilesFound: 4}, SeverityInfo, "could not be measured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findingWith(t, Diagnose(codexAgent(), healthy, tc.adoption, time.Now()), tc.substr)
			if got.Severity != tc.want {
				t.Errorf("severity=%s want %s (%q)", got.Severity, tc.want, got.Summary)
			}
		})
	}
}

// TestNoHooksConfiguredIsNotATrustProblem: with nothing installed there is
// nothing to trust, so the remedy is the installer.
func TestNoHooksConfiguredIsNotATrustProblem(t *testing.T) {
	agent := codexAgent()
	agent.Configured = map[string]int{}

	findings := Diagnose(agent, activity(nil), Adoption{}, time.Now())
	got := findingWith(t, findings, "no Gortex lifecycle hooks")
	if got.Severity != SeverityWarn || got.Remedy != "gortex install" {
		t.Errorf("got %+v", got)
	}
	assertNoFindingWith(t, findings, "trusted")
}
