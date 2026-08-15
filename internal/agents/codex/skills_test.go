package codex

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// TestCodexSkillsNeverLandUnderDotCodex is the test that separates a
// shipped feature from a green suite over nothing.
//
// Codex scans `.agents/skills` (cwd upward), `$HOME/.agents/skills` and
// `/etc/codex/skills` — and nothing else. `~/.codex` is configuration
// only. A SKILL.md written under `~/.codex/skills` or
// `<repo>/.codex/skills` is neither an error nor a warning: Codex never
// looks, so every positive assertion about "we wrote the skills" would
// still pass while the user's picker stayed empty. This test fails the
// moment an install starts writing to the dead drop.
func TestCodexSkillsNeverLandUnderDotCodex(t *testing.T) {
	for _, tc := range []struct {
		name   string
		newEnv func(*testing.T) agents.Env
	}{
		{"global", codexGlobalEnv},
		{"project", codexProjectEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.newEnv(t)
			if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
				t.Fatalf("apply: %v", err)
			}
			assertNoSkillsDir(t, filepath.Join(env.Home, ".codex", "skills"))
			assertNoSkillsDir(t, filepath.Join(env.Root, ".codex", "skills"))
		})
	}
}

// TestCodexGlobalModeInstallsCuratedSkillPack pins the curated pack to
// the shared `$HOME/.agents/skills` root — the one OpenCode reads too,
// so a single write serves both hosts.
func TestCodexGlobalModeInstallsCuratedSkillPack(t *testing.T) {
	env := codexGlobalEnv(t)
	res, err := New().Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}

	skills, err := Skills()
	if err != nil {
		t.Fatalf("skills: %v", err)
	}
	if len(skills) == 0 {
		t.Fatal("curated skill pack is empty")
	}
	for _, s := range skills {
		path := filepath.Join(env.Home, ".agents", "skills", s.ID, "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if got := string(data); got != renderSkill(s) {
			t.Fatalf("%s is not the rendered skill:\n%s", path, got)
		}
		if !reportsPath(res, path) {
			t.Fatalf("apply did not report %s in its file actions", path)
		}
	}

	// The curated pack is codebase-agnostic; `gortex install` must not
	// leak it into the repo the user happens to be standing in.
	assertNoSkillsDir(t, filepath.Join(env.Root, ".agents", "skills"))
}

// TestCodexProjectModeSkipsCuratedSkillPack keeps `gortex init` from
// dropping 21 host-agnostic markdown files into a user's working tree
// for their teammates to review. Same reason claudecode's
// SyncGlobalSkills lives inside applyGlobal.
func TestCodexProjectModeSkipsCuratedSkillPack(t *testing.T) {
	env := codexProjectEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertNoSkillsDir(t, filepath.Join(env.Home, ".agents", "skills"))
	for _, dir := range skillDirNames(t, filepath.Join(env.Root, ".agents", "skills")) {
		if dir == "gortex-explore" {
			t.Fatalf("curated skill %q leaked into the repo", dir)
		}
	}
}

// TestCodexProjectModeWritesGeneratedCommunitySkills covers the first
// non-Claude consumer of Env.GeneratedSkills. The content is passed
// through verbatim — the generator already renders a complete SKILL.md.
func TestCodexProjectModeWritesGeneratedCommunitySkills(t *testing.T) {
	env := codexProjectEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, s := range env.GeneratedSkills {
		path := filepath.Join(env.Root, ".agents", "skills", s.DirName, "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if string(data) != s.Content {
			t.Fatalf("%s content drifted from the generated skill:\n%s", path, data)
		}
	}
}

// TestCodexSkillsAreNeverNested guards the layout Codex documents: one
// directory per skill, directly under `.agents/skills`. Recursion into a
// deeper subdirectory is undocumented, so a `generated/` grouping level
// would be a silent gamble.
func TestCodexSkillsAreNeverNested(t *testing.T) {
	t.Run("global", func(t *testing.T) {
		env := codexGlobalEnv(t)
		if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		assertSkillDepth(t, filepath.Join(env.Home, ".agents", "skills"))
	})
	t.Run("project", func(t *testing.T) {
		env := codexProjectEnv(t)
		if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		assertSkillDepth(t, filepath.Join(env.Root, ".agents", "skills"))
	})
}

// TestCodexSkillFrontmatterIsNameAndDescriptionOnly pins the envelope.
// Codex uses those two keys and ignores the rest, and the file is shared
// with OpenCode — every extra key is one more way for two adapters to
// disagree byte-for-byte about a path they both write.
func TestCodexSkillFrontmatterIsNameAndDescriptionOnly(t *testing.T) {
	env := codexGlobalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	skills, err := Skills()
	if err != nil {
		t.Fatalf("skills: %v", err)
	}
	for _, s := range skills {
		path := filepath.Join(env.Home, ".agents", "skills", s.ID, "SKILL.md")
		keys := frontmatterKeys(t, path)
		if len(keys) != 2 || keys[0] != "name" || keys[1] != "description" {
			t.Fatalf("%s frontmatter keys=%v want [name description]", path, keys)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		// The Claude frontmatter must have been stripped, not stacked:
		// two blocks in one file makes most hosts drop the skill.
		if strings.Count(string(body), "\n---\n") > 1 {
			t.Fatalf("%s carries a second frontmatter fence:\n%s", path, body)
		}
	}
}

// TestCodexKeepsUserEditedCuratedSkill pins the never-clobber posture.
// `gortex install` runs again after every upgrade; a writer that
// regenerated these would silently discard the user's edits each time.
func TestCodexKeepsUserEditedCuratedSkill(t *testing.T) {
	env := codexGlobalEnv(t)
	a := New()
	if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	path := filepath.Join(env.Home, ".agents", "skills", "gortex-explore", "SKILL.md")
	const edited = "---\nname: gortex-explore\ndescription: my own wording\n---\n\n# Mine\n"
	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := a.Apply(env, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != edited {
		t.Fatalf("user-edited skill was overwritten:\n%s", data)
	}
	for _, f := range res.Files {
		if f.Path != path {
			continue
		}
		if f.Action != agents.ActionSkip || f.Reason != "customised" {
			t.Fatalf("action for %s = %s/%q want skip/customised", path, f.Action, f.Reason)
		}
	}
}

// TestCodexSkipsNonKebabGeneratedSkill: OpenCode and GitHub Copilot
// refuse a non-kebab skill name at load time, and refuse it silently.
// The same generated fixture flows to all three hosts, so a bad DirName
// is dropped loudly here rather than written once and ignored thrice.
func TestCodexSkipsNonKebabGeneratedSkill(t *testing.T) {
	env := codexProjectEnv(t)
	env.GeneratedSkills = []agents.GeneratedSkill{
		{CommunityID: "bad", Label: "Bad Skill", DirName: "Gortex_Bad Skill", Content: "---\nname: bad\n---\n# Bad\n"},
		{CommunityID: "good", Label: "good", DirName: "gortex-good", Content: "---\nname: gortex-good\n---\n# Good\n"},
	}
	buf := env.Stderr.(*bytes.Buffer)

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got := skillDirNames(t, filepath.Join(env.Root, ".agents", "skills"))
	if len(got) != 1 || got[0] != "gortex-good" {
		t.Fatalf("skill dirs=%v want [gortex-good]", got)
	}
	if !strings.Contains(buf.String(), "kebab-case") {
		t.Fatalf("expected a warning naming the kebab-case rule:\n%s", buf.String())
	}

	plan, err := New().Plan(env)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	for _, f := range plan.Files {
		if strings.Contains(f.Path, "Bad Skill") {
			t.Fatalf("Plan advertised a skill Apply refuses to write: %s", f.Path)
		}
	}
}

// TestCodexPlanEnumeratesEverySkillPath: `gortex doctor` and
// --print-config read Plan(), never Apply(), so a path missing from the
// plan is invisible to both — a user chasing a missing skill has nothing
// to compare against.
func TestCodexPlanEnumeratesEverySkillPath(t *testing.T) {
	for _, tc := range []struct {
		name   string
		newEnv func(*testing.T) agents.Env
	}{
		{"global", codexGlobalEnv},
		{"project", codexProjectEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := tc.newEnv(t)
			a := New()
			plan, err := a.Plan(env)
			if err != nil {
				t.Fatalf("plan: %v", err)
			}
			planned := make(map[string]bool, len(plan.Files))
			for _, f := range plan.Files {
				planned[f.Path] = true
			}

			res, err := a.Apply(env, agents.ApplyOpts{})
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			written := 0
			for _, f := range res.Files {
				if filepath.Base(f.Path) != "SKILL.md" {
					continue
				}
				written++
				if !planned[f.Path] {
					t.Fatalf("Apply wrote %s but Plan never listed it", f.Path)
				}
			}
			if written == 0 {
				t.Fatal("expected Apply to write at least one SKILL.md")
			}
		})
	}
}

// TestCodexSkillsIdempotent: a re-run must skip every skill, in both
// modes. `gortex install` / `gortex init` are re-run after every upgrade.
func TestCodexSkillsIdempotent(t *testing.T) {
	t.Run("global", func(t *testing.T) {
		env := codexGlobalEnv(t)
		a := New()
		res, err := a.Apply(env, agents.ApplyOpts{})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{
			agents.ActionCreate: 1 + curatedSkillCount(t) + subAgentCount(),
		})
		agentstest.AssertIdempotent(t, a, env)
	})
	t.Run("project", func(t *testing.T) {
		env := codexProjectEnv(t)
		a := New()
		res, err := a.Apply(env, agents.ApplyOpts{})
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		agentstest.AssertCountsByAction(t, res, map[agents.ActionKind]int{
			agents.ActionCreate: 2 + len(env.GeneratedSkills),
		})
		agentstest.AssertIdempotent(t, a, env)
	})
}

// TestCodexSkillsDryRunTouchesNothing: --dry-run must plan the pack
// without materialising it.
func TestCodexSkillsDryRunTouchesNothing(t *testing.T) {
	env := codexGlobalEnv(t)
	res, err := New().Apply(env, agents.ApplyOpts{DryRun: true})
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertNoSkillsDir(t, filepath.Join(env.Home, ".agents", "skills"))
	wouldCreate := 0
	for _, f := range res.Files {
		if filepath.Base(f.Path) == "SKILL.md" && f.Action == agents.ActionWouldCreate {
			wouldCreate++
		}
	}
	if wouldCreate != curatedSkillCount(t) {
		t.Fatalf("dry-run planned %d skills, want %d", wouldCreate, curatedSkillCount(t))
	}
}

// profileSubset is the three-skill shape an instruction profile like
// `localization` narrows the pack to.
var profileSubset = []string{"gortex-explore", "gortex-guide", "gortex-debug"}

// TestCodexSyncSkillsNarrowsToProfileSubset is the point of the whole
// exercise: `gortex instructions switch localization` must leave Codex
// holding three playbooks, not twenty-one. Before this existed, only
// Claude Code's picker shrank and every other host kept the full pack.
func TestCodexSyncSkillsNarrowsToProfileSubset(t *testing.T) {
	env := codexGlobalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	root := filepath.Join(env.Home, ".agents", "skills")
	if got := len(skillDirNames(t, root)); got != curatedSkillCount(t) {
		t.Fatalf("install left %d skills, want the full pack (%d)", got, curatedSkillCount(t))
	}

	if _, err := SyncSkills(env.Stderr, env.Home, profileSubset, agents.ApplyOpts{}); err != nil {
		t.Fatalf("SyncSkills: %v", err)
	}
	want := append([]string(nil), profileSubset...)
	sort.Strings(want)
	if got := skillDirNames(t, root); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("after the switch %v remain, want %v", got, want)
	}
}

// TestCodexSyncSkillsKeepsCustomisedSkill: a profile is not worth a
// user's edits. An out-of-profile playbook the user rewrote stays, byte
// for byte, and the switch says so instead of silently shipping a wider
// surface than requested.
func TestCodexSyncSkillsKeepsCustomisedSkill(t *testing.T) {
	env := codexGlobalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	root := filepath.Join(env.Home, ".agents", "skills")
	path := filepath.Join(root, "gortex-rename", "SKILL.md")
	const mine = "---\nname: gortex-rename\ndescription: mine\n---\n\n# my own steps\n"
	if err := os.WriteFile(path, []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}

	log := &bytes.Buffer{}
	if _, err := SyncSkills(log, env.Home, profileSubset, agents.ApplyOpts{}); err != nil {
		t.Fatalf("SyncSkills: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("customised skill was deleted to satisfy a profile: %v", err)
	}
	if string(data) != mine {
		t.Fatalf("customised skill was rewritten:\n%s", data)
	}
	if !strings.Contains(log.String(), "gortex-rename") {
		t.Fatalf("expected a warning naming the kept skill, got:\n%s", log.String())
	}
}

// TestCodexSyncSkillsWithoutAnInstalledRootIsANoOp: most machines run
// one or two of the supported hosts. Switching a profile must not
// conjure a Codex skills tree on a machine that never ran `gortex
// install`, and must not fail because the tree is absent.
func TestCodexSyncSkillsWithoutAnInstalledRootIsANoOp(t *testing.T) {
	env := codexGlobalEnv(t)
	actions, err := SyncSkills(env.Stderr, env.Home, profileSubset, agents.ApplyOpts{})
	if err != nil {
		t.Fatalf("SyncSkills on an uninstalled machine: %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("actions = %v, want none", actions)
	}
	assertNoSkillsDir(t, filepath.Join(env.Home, ".agents", "skills"))
}

// codexProjectEnv is the ModeProject counterpart to codexGlobalEnv:
// codex detected via ~/.codex, a stubbed version probe, and the
// generated-skill fixture agentstest seeds.
func codexProjectEnv(t *testing.T) agents.Env {
	t.Helper()
	t.Setenv(codexHookModeEnvVar, "")
	stubCodexVersion(t, "codex-cli 0.147.0\n")
	env, _ := agentstest.NewEnv(t)
	if err := os.MkdirAll(filepath.Join(env.Home, ".codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	return env
}

// curatedSkillCount is how many SKILL.md files a ModeGlobal Apply
// writes. Derived, not hardcoded, so adding a playbook to the shipped
// pack does not mean editing every count assertion in this package.
func curatedSkillCount(t *testing.T) int {
	t.Helper()
	skills, err := Skills()
	if err != nil {
		t.Fatalf("skills: %v", err)
	}
	return len(skills)
}

// assertNoSkillsDir fails when dir exists at all — an empty directory we
// created is still a directory the user has to wonder about.
func assertNoSkillsDir(t *testing.T, dir string) {
	t.Helper()
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return
	} else if err != nil {
		t.Fatalf("stat %s: %v", dir, err)
	}
	var found []string
	if err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found = append(found, path)
		}
		return nil
	}); err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	t.Fatalf("nothing should be written to %s; found dir with files %v", dir, found)
}

// assertSkillDepth requires every file under root to sit exactly one
// directory deep: <root>/<skill>/SKILL.md.
func assertSkillDepth(t *testing.T, root string) {
	t.Helper()
	seen := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		parts := strings.Split(rel, string(filepath.Separator))
		if len(parts) != 2 || parts[1] != "SKILL.md" {
			t.Fatalf("skill file %s is not <skill>/SKILL.md directly under %s", rel, root)
		}
		seen++
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if seen == 0 {
		t.Fatalf("no skill files under %s", root)
	}
}

// skillDirNames lists the immediate child directory names of root, or
// nothing when root does not exist.
func skillDirNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read dir %s: %v", root, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// frontmatterKeys returns the top-level YAML keys of path's leading
// frontmatter block, in file order.
func frontmatterKeys(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	lines := strings.Split(string(data), "\n")
	if len(lines) == 0 || lines[0] != "---" {
		t.Fatalf("%s does not open with a frontmatter fence:\n%s", path, data)
	}
	var keys []string
	for _, line := range lines[1:] {
		if line == "---" {
			return keys
		}
		key, _, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("%s has a non key/value frontmatter line %q", path, line)
		}
		keys = append(keys, key)
	}
	t.Fatalf("%s has an unterminated frontmatter block:\n%s", path, data)
	return nil
}

// reportsPath reports whether res mentions path in its file actions.
func reportsPath(res *agents.Result, path string) bool {
	for _, f := range res.Files {
		if f.Path == path {
			return true
		}
	}
	return false
}
