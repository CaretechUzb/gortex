package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/zzet/gortex/internal/profiles"
)

// sandboxInstructionsEnv points the profile dir (XDG_DATA_HOME) and the
// home dir (skills sync, pointer nudge) at temp dirs, and neutralises
// the profile env override so on-disk state decides.
func sandboxInstructionsEnv(t *testing.T) (home, dataDir string) {
	t.Helper()
	home = t.TempDir()
	dataDir = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", dataDir)
	t.Setenv(profiles.ActiveEnv, "")
	return home, dataDir
}

func captureCmd() (*cobra.Command, *bytes.Buffer, *bytes.Buffer) {
	cmd := &cobra.Command{}
	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd, out, errOut
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
