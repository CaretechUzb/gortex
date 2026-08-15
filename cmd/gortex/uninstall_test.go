package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/opencode"
	"github.com/zzet/gortex/internal/testenv"
)

// seedUninstallSandbox builds a repo that looks like `gortex init` ran in
// it AND like a team has been writing its own skills in the same trees.
// The user's files are what the test is really about: the three skill
// roots are shared, so a removal that reasons about directories instead of
// ownership destroys them.
func seedUninstallSandbox(t *testing.T) (repo string, userSkills []string) {
	t.Helper()
	repo = t.TempDir()
	t.Chdir(repo)

	write := func(rel, body string) string {
		path := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}

	write(".mcp.json", `{"mcpServers":{"gortex":{"command":"gortex","args":["mcp"]}}}`)
	write(".opencode/plugin/gortex.js", "// gortex bridge\n")
	for _, tree := range []string{".agents/skills", ".opencode/skills", ".github/skills"} {
		write(filepath.Join(tree, "gortex-stub", "SKILL.md"), "---\nname: gortex-stub\n---\n\ngenerated\n")
	}
	userSkills = []string{
		write(filepath.Join(".agents/skills", "team-runbook", "SKILL.md"), "---\nname: team-runbook\n---\n\nours\n"),
		write(filepath.Join(".opencode/skills", "deploy-notes", "SKILL.md"), "---\nname: deploy-notes\n---\n\nours\n"),
		write(filepath.Join(".github/skills", "handwritten", "SKILL.md"), "---\nname: handwritten\n---\n\nours\n"),
	}
	return repo, userSkills
}

func pinUninstallFlags(t *testing.T) {
	t.Helper()
	yes, global, purge, quiet := uninstallYes, uninstallGlobal, uninstallPurge, noProgress
	t.Cleanup(func() {
		uninstallYes, uninstallGlobal, uninstallPurge, noProgress = yes, global, purge, quiet
	})
	uninstallYes, uninstallGlobal, uninstallPurge, noProgress = true, false, false, true
}

// TestUninstallRemovesOnlyGortexEntriesFromSharedSkillTrees is the guard
// against the tempting one-line version of this feature: adding the three
// skill roots to uninstallDirs. That would pass every "no gortex-* left"
// assertion and delete the team's own skills on the way.
func TestUninstallRemovesOnlyGortexEntriesFromSharedSkillTrees(t *testing.T) {
	repo, userSkills := seedUninstallSandbox(t)
	pinUninstallFlags(t)

	before := map[string][]byte{}
	for _, path := range userSkills {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = body
	}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := runUninstall(cmd, nil); err != nil {
		t.Fatalf("uninstall: %v", err)
	}

	for _, gone := range []string{
		".mcp.json",
		".opencode/plugin/gortex.js",
		".agents/skills/gortex-stub",
		".opencode/skills/gortex-stub",
		".github/skills/gortex-stub",
	} {
		if _, err := os.Stat(filepath.Join(repo, gone)); err == nil {
			t.Errorf("%s survived uninstall", gone)
		}
	}
	for path, want := range before {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("a user skill was deleted: %v", err)
		}
		if string(got) != string(want) {
			t.Errorf("a user skill changed:\n%s", got)
		}
	}
}

// TestUninstallPrunesASharedTreeItEmptied: pruning is conditional on the
// tree being empty, which is the same predicate that keeps a user's skill
// alive. os.Remove refusing a non-empty directory is what enforces it.
func TestUninstallPrunesASharedTreeItEmptied(t *testing.T) {
	repo := t.TempDir()
	t.Chdir(repo)
	pinUninstallFlags(t)

	path := filepath.Join(repo, ".opencode", "skills", "gortex-stub", "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	if err := runUninstall(cmd, nil); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".opencode", "skills")); err == nil {
		t.Error("a skills tree holding nothing but ours should be pruned")
	}
}

// TestUninstallPreviewIsExactlyWhatGetsDeleted pins the wizard's promise.
// The confirm screen is the only thing standing between the user and an
// irreversible delete, so it may neither name a file removal will keep nor
// stay silent about one it will take.
func TestUninstallPreviewIsExactlyWhatGetsDeleted(t *testing.T) {
	repo, _ := seedUninstallSandbox(t)

	files, dirs := filterPresentUninstallTargets()
	previewed := append(append([]string{}, files...), dirs...)
	slices.Sort(previewed)

	// FromSlash because the owned-entry targets are built with
	// filepath.Join, which separates with a backslash on Windows.
	var want []string
	for _, rel := range []string{
		".agents/skills/gortex-stub",
		".github/skills/gortex-stub",
		".mcp.json",
		".opencode/plugin/gortex.js",
		".opencode/skills/gortex-stub",
	} {
		want = append(want, filepath.FromSlash(rel))
	}
	slices.Sort(want)
	if !slices.Equal(previewed, want) {
		t.Fatalf("preview = %v, want %v", previewed, want)
	}

	removed, failures := executeUninstall(files, dirs)
	if len(failures) != 0 {
		t.Fatalf("uninstall failures: %v", failures)
	}
	if removed != len(want) {
		t.Errorf("removed %d items, previewed %d", removed, len(want))
	}
	for _, rel := range want {
		if _, err := os.Stat(filepath.Join(repo, rel)); err == nil {
			t.Errorf("%s was previewed but survived", rel)
		}
	}
}

// TestGlobalUninstallArtifactsCoversEveryHost is the wiring guard for the
// user-level half: the preview used to be claudecode's alone, so a machine
// with only OpenCode configured reported "nothing to uninstall" and the
// removal branch never ran.
func TestGlobalUninstallArtifactsCoversEveryHost(t *testing.T) {
	home := t.TempDir()
	testenv.SandboxAt(t, home)

	env := agents.Env{
		Root:            t.TempDir(),
		Home:            home,
		HookCommand:     "/tmp/test-gortex hook",
		Mode:            agents.ModeGlobal,
		InstallHooks:    true,
		InstructionsDir: filepath.Join(home, ".gortex", "instructions"),
	}
	if _, err := opencode.New().Apply(env, agents.ApplyOpts{ForceDetect: true}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	got := globalUninstallArtifacts(home)
	if !slices.Contains(got, opencode.PluginPath(home)) {
		t.Errorf("the global preview misses the OpenCode bridge; got %v", got)
	}

	for _, host := range globalHosts() {
		removed, failures := host.remove(env, agents.ApplyOpts{})
		if len(failures) != 0 {
			t.Errorf("remove failures: %v", failures)
		}
		_ = removed
	}
	if got := globalUninstallArtifacts(home); len(got) != 0 {
		t.Errorf("the global footprint should be empty after removal, got %v", got)
	}
}
