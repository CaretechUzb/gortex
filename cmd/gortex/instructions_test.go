package main

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/claudecode"
	"github.com/zzet/gortex/internal/agents/codex"
	"github.com/zzet/gortex/internal/agents/opencode"
	"github.com/zzet/gortex/internal/profiles"
	"github.com/zzet/gortex/internal/testenv"
)

// sandboxInstructionsEnv points the profile dir (XDG_DATA_HOME) and the
// home dir at temp dirs, and neutralises the profile env override so
// on-disk state decides. The home has to be isolated the cross-platform
// way: runInstructionsSwitch hands os.UserHomeDir() to SyncGlobalSkills,
// which prunes shipped skills out of <home>/.claude/skills — against the
// developer's real profile on Windows, where a HOME-only override is a
// no-op.
func sandboxInstructionsEnv(t *testing.T) (home, dataDir string) {
	t.Helper()
	d := testenv.Sandbox(t)
	t.Setenv(profiles.ActiveEnv, "")
	return d.Home, d.Data
}

func captureCmd() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd, out, errOut
}

// installedSkillIDs lists the skill directories under root that still
// hold a SKILL.md — what a host would actually load, as opposed to what
// directories happen to exist.
func installedSkillIDs(t *testing.T, root string) []string {
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
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(root, e.Name(), "SKILL.md")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// TestInstructionsSwitchReshapesEverySkillHost is the regression guard
// for the bug this wiring closes: `instructions switch` used to prune
// only ~/.claude/skills, so a user who picked a three-skill profile kept
// the full twenty-one-playbook surface on every other host they had
// installed — the profile was a lie everywhere but Claude Code.
//
// The sandbox seeds three hosts by running each adapter's own sync with
// a nil subset (the install path), so the seeded bytes are exactly what
// that host ships and the prune decision is exercised for real rather
// than against a fixture.
func TestInstructionsSwitchReshapesEverySkillHost(t *testing.T) {
	home, _ := sandboxInstructionsEnv(t)
	// Both of these relocate a host's config root when exported, and the
	// sandbox home reads as the machine's real home, so a developer's own
	// value would redirect the prune outside the sandbox.
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("COPILOT_HOME", "")

	claudeRoot := filepath.Join(home, ".claude", "skills")
	codexRoot := filepath.Join(home, ".agents", "skills")
	opencodeRoot := filepath.Join(home, ".config", "opencode", "skills")

	// Seed each host's full pack. The two newer adapters no-op unless
	// their root already exists — a switch must never install a host —
	// so the directory is created first, exactly as `gortex install`
	// would have left it.
	for _, root := range []string{codexRoot, opencodeRoot} {
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, seed := range []struct {
		name string
		sync func(w *bytes.Buffer, home string) error
	}{
		{"claude code", func(w *bytes.Buffer, home string) error {
			_, err := claudecode.SyncGlobalSkills(w, home, nil, agents.ApplyOpts{})
			return err
		}},
		{"codex", func(w *bytes.Buffer, home string) error {
			_, err := codex.SyncSkills(w, home, nil, agents.ApplyOpts{})
			return err
		}},
		{"opencode", func(w *bytes.Buffer, home string) error {
			_, err := opencode.SyncSkills(w, home, nil, agents.ApplyOpts{})
			return err
		}},
	} {
		if err := seed.sync(&bytes.Buffer{}, home); err != nil {
			t.Fatalf("seed %s: %v", seed.name, err)
		}
	}

	full := installedSkillIDs(t, claudeRoot)
	if len(full) < 4 {
		t.Fatalf("seeded pack is %v — too small for a subset to prove anything", full)
	}
	for _, root := range []string{codexRoot, opencodeRoot} {
		if got := installedSkillIDs(t, root); len(got) != len(full) {
			t.Fatalf("%s seeded with %d skills, want the full pack (%d)", root, len(got), len(full))
		}
	}

	// One playbook the user rewrote, outside the target profile. It must
	// survive on every host: enforcing a profile is worth deleting files
	// Gortex wrote, never a file the user did.
	const mine = "---\nname: gortex-rename\ndescription: mine\n---\nmy own steps\n"
	for _, root := range []string{claudeRoot, codexRoot, opencodeRoot} {
		if err := os.WriteFile(filepath.Join(root, "gortex-rename", "SKILL.md"), []byte(mine), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd, out, _ := captureCmd()
	if err := runInstructionsSwitch(cmd, []string{"localization"}); err != nil {
		t.Fatalf("switch: %v", err)
	}

	loc, _ := profiles.ByName("localization")
	if loc.Skills == nil {
		t.Fatal("the localization profile no longer carries a skills subset — this test proves nothing")
	}
	want := append([]string(nil), loc.Skills...)
	want = append(want, "gortex-rename") // kept: customised
	sort.Strings(want)

	for _, root := range []string{claudeRoot, codexRoot, opencodeRoot} {
		if got := installedSkillIDs(t, root); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s holds %v after the switch, want %v", root, got, want)
		}
		path := filepath.Join(root, "gortex-rename", "SKILL.md")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: customised skill was deleted to satisfy a profile: %v", path, err)
			continue
		}
		if string(data) != mine {
			t.Errorf("%s: customised skill was rewritten:\n%s", path, data)
		}
	}

	if !strings.Contains(out.String(), "Skills reconciled") {
		t.Errorf("switch must report the reconciliation, got:\n%s", out.String())
	}

	// Switching back must restore the full pack on every host. A profile
	// that can only ever shrink the surface would make `localization` a
	// one-way door, and the user's customised playbook still has to
	// survive the return trip.
	cmd, _, _ = captureCmd()
	if err := runInstructionsSwitch(cmd, []string{"core"}); err != nil {
		t.Fatalf("switch back: %v", err)
	}
	for _, root := range []string{claudeRoot, codexRoot, opencodeRoot} {
		if got := installedSkillIDs(t, root); strings.Join(got, ",") != strings.Join(full, ",") {
			t.Errorf("%s holds %v after switching back to core, want the full pack %v", root, got, full)
		}
		if got, err := os.ReadFile(filepath.Join(root, "gortex-rename", "SKILL.md")); err != nil || string(got) != mine {
			t.Errorf("%s: customised skill did not survive the round trip (err=%v)", root, err)
		}
	}
}

// TestInstructionsSwitchLeavesUninstalledHostsAlone: most machines run
// one or two of the supported hosts. A switch must not materialise a
// Codex / OpenCode / Copilot skills tree the user never asked for, and
// must not error because those trees are absent.
func TestInstructionsSwitchLeavesUninstalledHostsAlone(t *testing.T) {
	home, _ := sandboxInstructionsEnv(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("COPILOT_HOME", "")

	cmd, _, _ := captureCmd()
	if err := runInstructionsSwitch(cmd, []string{"localization"}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	for _, root := range []string{
		filepath.Join(home, ".agents", "skills"),
		filepath.Join(home, ".config", "opencode", "skills"),
		filepath.Join(home, ".copilot", "skills"),
	} {
		if _, err := os.Stat(root); !os.IsNotExist(err) {
			t.Errorf("switch created %s on a machine with no such host (stat err = %v)", root, err)
		}
	}
}

func TestInstructionsSwitchListShowRegen(t *testing.T) {
	_, _ = sandboxInstructionsEnv(t)
	dir := profiles.DefaultDir()

	// switch → files land, state recorded, caveat printed.
	cmd, out, _ := captureCmd()
	if err := runInstructionsSwitch(cmd, []string{"localization"}); err != nil {
		t.Fatalf("switch: %v", err)
	}
	text := out.String()
	if !strings.Contains(text, "NEW sessions only") {
		t.Errorf("switch output must state the next-session caveat, got:\n%s", text)
	}
	if !strings.Contains(text, "gortex install") {
		t.Errorf("switch on an uninstalled machine should nudge toward `gortex install`, got:\n%s", text)
	}
	active, err := os.ReadFile(filepath.Join(dir, profiles.ActiveFileName))
	if err != nil {
		t.Fatalf("active.md missing after switch: %v", err)
	}
	loc, _ := profiles.ByName("localization")
	if string(active) != loc.Body() {
		t.Error("active.md is not the switched profile body")
	}

	// list → active marker on the switched profile.
	cmd, out, _ = captureCmd()
	if err := runInstructionsList(cmd, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "* localization") {
		t.Errorf("list must mark the active profile, got:\n%s", out.String())
	}

	// show → exact body on stdout.
	cmd, out, _ = captureCmd()
	if err := runInstructionsShow(cmd, []string{"core"}); err != nil {
		t.Fatalf("show: %v", err)
	}
	core, _ := profiles.ByName("core")
	if out.String() != core.Body() {
		t.Error("show did not print the exact profile body")
	}
	if err := runInstructionsShow(cmd, []string{"nope"}); err == nil {
		t.Error("show of an unknown profile must error")
	}

	// regen → keeps the active selection.
	cmd, out, _ = captureCmd()
	if err := runInstructionsRegen(cmd, nil); err != nil {
		t.Fatalf("regen: %v", err)
	}
	if !strings.Contains(out.String(), "active: localization") {
		t.Errorf("regen must preserve the active selection, got:\n%s", out.String())
	}
}
