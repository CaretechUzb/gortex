package claudecode

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

func TestProjectCommunityRoutingUsesCanonicalAgentsFile(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !res.Configured {
		t.Fatal("expected Configured=true")
	}

	agentsBody := readProjectInstructions(t, filepath.Join(env.Root, "AGENTS.md"))
	for _, want := range []string{agents.CommunitiesStartMarker, agentstest.StubSkillsRouting, agents.CommunitiesEndMarker} {
		if !strings.Contains(agentsBody, want) {
			t.Errorf("AGENTS.md missing %q:\n%s", want, agentsBody)
		}
	}
	claudeBody := readProjectInstructions(t, filepath.Join(env.Root, "CLAUDE.md"))
	if claudeBody != "@AGENTS.md\n" {
		t.Fatalf("CLAUDE.md = %q, want only canonical import", claudeBody)
	}
}

func TestProjectCommunityRoutingMigratesManagedClaudeBlockAndPreservesUserContent(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	claudePath := filepath.Join(env.Root, "CLAUDE.md")
	agentsPath := filepath.Join(env.Root, "AGENTS.md")
	oldClaude := "# User preface\n\n" + agents.CommunitiesStartMarker + "\nold generated routing\n" + agents.CommunitiesEndMarker + "\n\n## Claude Code\nKeep this section.\n"
	if err := os.WriteFile(claudePath, []byte(oldClaude), 0o644); err != nil {
		t.Fatalf("seed CLAUDE.md: %v", err)
	}
	if err := os.WriteFile(agentsPath, []byte("# Team rules\nKeep these rules.\n"), 0o644); err != nil {
		t.Fatalf("seed AGENTS.md: %v", err)
	}

	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	claudeBody := readProjectInstructions(t, claudePath)
	for _, want := range []string{"@AGENTS.md", "# User preface", "## Claude Code", "Keep this section."} {
		if !strings.Contains(claudeBody, want) {
			t.Errorf("CLAUDE.md lost %q:\n%s", want, claudeBody)
		}
	}
	for _, unwanted := range []string{agents.CommunitiesStartMarker, agents.CommunitiesEndMarker, "old generated routing", agentstest.StubSkillsRouting} {
		if strings.Contains(claudeBody, unwanted) {
			t.Errorf("CLAUDE.md retained %q:\n%s", unwanted, claudeBody)
		}
	}
	if got := strings.Count(claudeBody, "@AGENTS.md"); got != 1 {
		t.Fatalf("CLAUDE.md import count = %d, want 1:\n%s", got, claudeBody)
	}

	agentsBody := readProjectInstructions(t, agentsPath)
	for _, want := range []string{"# Team rules", "Keep these rules.", agents.CommunitiesStartMarker, agentstest.StubSkillsRouting} {
		if !strings.Contains(agentsBody, want) {
			t.Errorf("AGENTS.md lost %q:\n%s", want, agentsBody)
		}
	}

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	for _, action := range res.Files {
		if action.Action != agents.ActionSkip {
			t.Errorf("second apply action for %s = %s, want skip", action.Path, action.Action)
		}
	}
}

func TestProjectCommunityRoutingDoesNotDuplicateExistingImport(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	claudePath := filepath.Join(env.Root, "CLAUDE.md")
	original := "@AGENTS.md\n\n## Claude Code\nClaude-only policy.\n"
	if err := os.WriteFile(claudePath, []byte(original), 0o644); err != nil {
		t.Fatalf("seed CLAUDE.md: %v", err)
	}
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	body := readProjectInstructions(t, claudePath)
	if got := strings.Count(body, "@AGENTS.md"); got != 1 {
		t.Fatalf("CLAUDE.md import count = %d, want 1:\n%s", got, body)
	}
	if !strings.Contains(body, "Claude-only policy.") {
		t.Fatalf("CLAUDE.md lost Claude-specific content:\n%s", body)
	}
}

func TestProjectAnalyzeOverviewStaysInClaudeWhileRoutingMovesToAgents(t *testing.T) {
	env, _ := agentstest.NewEnv(t)
	env.AnalyzedOverview = "## Repository Overview\nClaude-specific overview."
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	claudeBody := readProjectInstructions(t, filepath.Join(env.Root, "CLAUDE.md"))
	for _, want := range []string{"@AGENTS.md", agents.CommunitiesStartMarker, env.AnalyzedOverview, agents.CommunitiesEndMarker} {
		if !strings.Contains(claudeBody, want) {
			t.Errorf("CLAUDE.md missing %q:\n%s", want, claudeBody)
		}
	}
	if strings.Contains(claudeBody, agentstest.StubSkillsRouting) {
		t.Fatalf("CLAUDE.md duplicated community routing:\n%s", claudeBody)
	}
	if agentsBody := readProjectInstructions(t, filepath.Join(env.Root, "AGENTS.md")); !strings.Contains(agentsBody, agentstest.StubSkillsRouting) {
		t.Fatalf("AGENTS.md missing community routing:\n%s", agentsBody)
	}
}

func readProjectInstructions(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Base(path), err)
	}
	return string(body)
}
