package copilotcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

// surfaces_test.go is the retired-discovery-root fence. Every directory
// below is somewhere a maintainer could plausibly install a skill, an
// agent or a command — and every one of them is dead for the Copilot CLI.
// A write there costs nothing at install time and produces exactly
// nothing at runtime, with no error to explain it, so the only way to
// keep it from creeping back in is to assert its absence.

// deadSurfaces returns the paths this adapter must never create, with the
// vendor reason attached so a failure explains itself.
func deadSurfaces(env agents.Env) map[string]string {
	return map[string]string{
		filepath.Join(env.Home, ".claude", "skills"):   "the CLI stopped loading ~/.claude/skills in v1.0.36",
		filepath.Join(env.Home, ".claude", "agents"):   "the CLI stopped loading ~/.claude/agents in v1.0.36",
		filepath.Join(env.Home, ".claude", "commands"): "the CLI stopped loading ~/.claude/commands in v1.0.36",
		filepath.Join(env.Home, ".claude"):             "no home-level Claude directory is read since v1.0.36",
		filepath.Join(env.Home, ".agents", "skills"):   "the CLI stopped loading ~/.agents/skills in v1.0.66",
		filepath.Join(env.Root, ".claude"):             "the repo-local Claude tree belongs to the claudecode adapter",
		filepath.Join(env.Root, ".agents"):             "the repo-local .agents tree is not this adapter's to own",
		filepath.Join(env.Root, ".github", "prompts"):  "prompt files are a VS Code chat feature the CLI has never supported",
		filepath.Join(env.Root, ".github", "agents"):   "a home agent shadows a repo agent, and a repo copy is committed",
		filepath.Join(env.Root, ".github", "hooks"):    "a repo hook file is committed and fires on every teammate's clone",
	}
}

func TestWritesNothingToRetiredSurfaces(t *testing.T) {
	modes := map[string]agents.Mode{"project": agents.ModeProject, "global": agents.ModeGlobal}
	for name, mode := range modes {
		t.Run(name, func(t *testing.T) {
			env, _ := newCopilotEnv(t)
			env.Mode = mode
			env.InstallGlobalInstructions = true
			if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
				t.Fatalf("apply: %v", err)
			}
			for dir, why := range deadSurfaces(env) {
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Errorf("%s must not be written — %s (stat err = %v)", dir, why, err)
				}
			}
		})
	}
}

// TestPlanNeverNamesARetiredSurface: doctor and --print-config read Plan,
// so a dead path promised there is a dead path a user goes looking for.
func TestPlanNeverNamesARetiredSurface(t *testing.T) {
	modes := []agents.Mode{agents.ModeProject, agents.ModeGlobal}
	for _, mode := range modes {
		env, _ := newCopilotEnv(t)
		env.Mode = mode
		env.InstallGlobalInstructions = true
		plan, err := New().Plan(env)
		if err != nil {
			t.Fatalf("plan: %v", err)
		}
		dead := deadSurfaces(env)
		for _, f := range plan.Files {
			for dir, why := range dead {
				if f.Path == dir || isUnder(f.Path, dir) {
					t.Errorf("Plan names %s under a retired surface — %s", f.Path, why)
				}
			}
		}
	}
}

// isUnder reports whether path lives inside ancestor.
func isUnder(path, ancestor string) bool {
	rel, err := filepath.Rel(ancestor, path)
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
