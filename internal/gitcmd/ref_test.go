package gitcmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateRef pins the rule that closes the option-injection class:
// argv carries no quoting, so a revision beginning with "-" is parsed by git
// as a flag rather than a ref.
func TestValidateRef(t *testing.T) {
	valid := []string{
		"", // omitted — call sites never place it in argv
		"main",
		"HEAD",
		"HEAD~3",
		"HEAD^{commit}",
		"origin/main",
		"refs/heads/feature",
		"v1.2.3",
		"a1b2c3d4",
		"main...HEAD",
		"HEAD:path/to/file.go",
		"release/2026-08",
		"fix/some-branch", // an interior "-" is fine; only a leading one is not
	}
	for _, ref := range valid {
		if err := ValidateRef(ref); err != nil {
			t.Errorf("ValidateRef(%q) = %v, want nil — this is a legitimate revision", ref, err)
		}
	}

	invalid := []string{
		"--output=/tmp/pwned", // writes an arbitrary file
		"--git-dir=/etc",      // repoints git at another repository
		"--work-tree=/",
		"--upload-pack=/bin/sh",
		"-c",
		"-",
		"--output=/tmp/x...HEAD", // the shape GitDiffArgs builds
		"main branch",            // whitespace
		"main\nlog",              // newline: corrupts logs and error strings
		"main\ttab",
		"main\x00null",
	}
	for _, ref := range invalid {
		if err := ValidateRef(ref); err == nil {
			t.Errorf("ValidateRef(%q) = nil, want an error", ref)
		}
	}
}

// TestSafeRef checks the degrade-to-default helper used where the call site
// has no error return.
func TestSafeRef(t *testing.T) {
	if got := SafeRef("main"); got != "main" {
		t.Errorf("SafeRef(main) = %q, want main", got)
	}
	if got := SafeRef("--output=/tmp/pwned"); got != "" {
		t.Errorf("SafeRef of a hostile ref = %q, want \"\" so the caller falls back to its default", got)
	}
}

// TestValidateRef_BlocksRealGitFileWrite is the end-to-end proof. It runs the
// exact argv shape the churn enricher builds and confirms two things: that git
// really does honour an option-shaped revision by writing a file (so the
// validator is guarding a live behaviour, not a theoretical one), and that
// ValidateRef refuses that input before it can be built.
//
// Skipped when git is unavailable, or when this git is old enough not to
// support `log --output` — in which case there is nothing to demonstrate.
func TestValidateRef_BlocksRealGitFileWrite(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	repo := t.TempDir()
	run := func(args ...string) error {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		return cmd.Run()
	}
	if err := run("init", "-q"); err != nil {
		t.Skipf("git init failed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := run("add", "-A"); err != nil {
		t.Fatal(err)
	}
	if err := run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "init"); err != nil {
		t.Skipf("git commit failed: %v", err)
	}

	target := filepath.Join(t.TempDir(), "written-by-git.txt")
	hostileRef := "--output=" + target

	// The validator must refuse it. This is the assertion that matters.
	if err := ValidateRef(hostileRef); err == nil {
		t.Fatalf("ValidateRef(%q) accepted an option-shaped revision", hostileRef)
	} else if !strings.Contains(err.Error(), "option") {
		t.Errorf("error should explain WHY it is refused, got: %v", err)
	}

	// Demonstrate the behaviour being guarded: the same token, placed where
	// fileCommits places it, makes git write the file.
	_ = run("log", hostileRef, "--no-merges", "--format=%H", "--", "README.md")
	if _, err := os.Stat(target); err != nil {
		t.Skipf("this git does not honour `log --output` (%v) — nothing to demonstrate, "+
			"but ValidateRef still refuses the input", err)
	}
	t.Logf("confirmed: git wrote %s when handed an option-shaped revision; ValidateRef refuses it", target)
}
