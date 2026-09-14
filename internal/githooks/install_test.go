package githooks

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// initRepo creates a fresh git repo at tmp and returns the root path.
// core.hooksPath is pinned to a repo-local "hooks" dir so HookPathFor
// (which reads merged local+global git config) can never resolve to a
// machine-global hooks dir — without this, running the suite on a host
// with a global core.hooksPath makes every install/uninstall test
// write to the real global hooks.
func initRepo(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	hooksDir := filepath.Join(tmp, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatalf("mkdir hooks: %v", err)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Tester"},
		{"config", "core.hooksPath", hooksDir},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmp
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return tmp
}

func TestInstallHookPostCommit_FreshFile(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true, RegenWiki: true, Binary: "gortex"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"#!/bin/sh",
		MarkerBegin,
		MarkerEnd,
		"gortex export --format mermaid",
		"gortex wiki",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat hook: %v", err)
	}
	// NTFS has no exec bit — every file there reports 0666 and os.Chmod
	// only toggles the read-only attribute, so the mode says nothing about
	// whether Git will run the hook. Git for Windows runs hooks through its
	// bundled sh regardless of permissions.
	if runtime.GOOS != "windows" {
		if mode := info.Mode().Perm(); mode&0o100 == 0 {
			t.Errorf("hook not executable: mode = %v", mode)
		}
	}
}

func TestInstallHookPostCommit_Idempotent(t *testing.T) {
	repo := initRepo(t)
	for i := range 3 {
		if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true}); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if c := strings.Count(got, MarkerBegin); c != 1 {
		t.Errorf("expected one MarkerBegin, got %d", c)
	}
	if c := strings.Count(got, MarkerEnd); c != 1 {
		t.Errorf("expected one MarkerEnd, got %d", c)
	}
}

func TestInstallHookPostCommit_PreservesUserContent(t *testing.T) {
	repo := initRepo(t)
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	preexisting := `#!/bin/sh
# my custom hook
echo "hello from user hook"
`
	if err := os.WriteFile(hookPath, []byte(preexisting), 0o755); err != nil {
		t.Fatalf("write preexisting: %v", err)
	}
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, `echo "hello from user hook"`) {
		t.Errorf("install should preserve user content; got:\n%s", got)
	}
	if !strings.Contains(got, MarkerBegin) {
		t.Errorf("install should add marker block; got:\n%s", got)
	}
}

func TestUninstallHookPostCommit_RemovesBlock(t *testing.T) {
	repo := initRepo(t)
	hookPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	preexisting := `#!/bin/sh
# my custom hook
echo "hello"
`
	if err := os.WriteFile(hookPath, []byte(preexisting), 0o755); err != nil {
		t.Fatalf("write preexisting: %v", err)
	}
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenWiki: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path, removed, err := UninstallHook(repo, "post-commit")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !removed {
		t.Error("Uninstall should report removed=true")
	}
	if path != hookPath {
		t.Errorf("Uninstall path mismatch: %q vs %q", path, hookPath)
	}
	body, err := os.ReadFile(hookPath)
	if err != nil {
		t.Fatalf("read hook after uninstall: %v", err)
	}
	got := string(body)
	if strings.Contains(got, MarkerBegin) || strings.Contains(got, MarkerEnd) {
		t.Errorf("Uninstall should remove markers; got:\n%s", got)
	}
	if !strings.Contains(got, `echo "hello"`) {
		t.Errorf("Uninstall should preserve user content; got:\n%s", got)
	}
}

func TestUninstallHookPostCommit_RemovesFileWhenStubOnly(t *testing.T) {
	repo := initRepo(t)
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenMermaid: true}); err != nil {
		t.Fatalf("Install: %v", err)
	}
	path, removed, err := UninstallHook(repo, "post-commit")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !removed {
		t.Error("expected removed=true on fresh-install uninstall")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected hook file removed; stat returned %v", err)
	}
}

func TestUninstallHookPostCommit_Noop(t *testing.T) {
	repo := initRepo(t)
	path, removed, err := UninstallHook(repo, "post-commit")
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if removed {
		t.Error("Uninstall on non-existent hook should report removed=false")
	}
	if path == "" {
		t.Error("Uninstall should still return resolved hook path")
	}
}

func TestInstallHook_PostMergeAndChurn(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-merge", InstallOpts{RegenChurn: true, ChurnBranch: "origin/main"})
	if err != nil {
		t.Fatalf("InstallHook post-merge: %v", err)
	}
	if filepath.Base(path) != "post-merge" {
		t.Errorf("expected post-merge hook file, got %s", path)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"# gortex-managed:post-merge:begin",
		"# gortex-managed:post-merge:end",
		"gortex enrich churn",
		`--branch="origin/main"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
	// Post-commit and post-merge should be independently managed.
	if _, err := InstallHook(repo, "post-commit", InstallOpts{RegenChurn: true}); err != nil {
		t.Fatalf("InstallHook post-commit: %v", err)
	}
	if _, removed, err := UninstallHook(repo, "post-merge"); err != nil || !removed {
		t.Fatalf("UninstallHook post-merge removed=%v err=%v", removed, err)
	}
	// Post-commit hook should still exist after we uninstalled post-merge.
	postCommitPath, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	if _, err := os.Stat(postCommitPath); err != nil {
		t.Errorf("post-commit hook should survive post-merge uninstall: %v", err)
	}
}

func TestInstallHook_RegenReleases(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-merge", InstallOpts{
		RegenReleases:  true,
		ReleasesBranch: "origin/main",
	})
	if err != nil {
		t.Fatalf("InstallHook post-merge: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"gortex enrich releases",
		`--branch="origin/main"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
}

func TestInstallHook_RejectsUnsupportedHook(t *testing.T) {
	repo := initRepo(t)
	if _, err := InstallHook(repo, "pre-push", InstallOpts{RegenMermaid: true}); err == nil {
		t.Fatal("expected error for unsupported hook pre-push")
	}
}

func TestInstallHook_BoundedCallsWrapped(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-merge", InstallOpts{
		RegenMermaid:       true,
		RegenWiki:          true,
		RegenDocs:          true,
		RegenChurn:         true,
		ChurnBranch:        "origin/main",
		RegenReleases:      true,
		ReleasesBranch:     "origin/main",
		HookTimeoutSeconds: 7,
	})
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	for _, want := range []string{
		"# Bound each gortex invocation so a busy daemon cannot hang git.",
		"gortex_hook_run() {",
		"command -v timeout",
		"command -v perl",
		`gortex_hook_run 7 gortex enrich churn --branch="origin/main" >/dev/null 2>&1 || true`,
		`gortex_hook_run 7 gortex enrich releases --branch="origin/main" >/dev/null 2>&1 || true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("hook missing %q. Body:\n%s", want, got)
		}
	}
	if c := strings.Count(got, "gortex_hook_run 7 "); c != 5 {
		t.Errorf("expected 5 wrapped invocations (mermaid, wiki, docs, churn, releases), got %d. Body:\n%s", c, got)
	}
	if c := strings.Count(got, "gortex_hook_run() {"); c != 1 {
		t.Errorf("expected exactly one helper definition, got %d", c)
	}
	if i, j := strings.Index(got, "gortex_hook_run() {"), strings.Index(got, "gortex_hook_run 7 "); i == -1 || j == -1 || i > j {
		t.Errorf("helper must be defined before the first wrapped call. Body:\n%s", got)
	}
	if strings.Contains(got, "(gortex enrich churn)") {
		t.Errorf("bounded install must not emit legacy unwrapped lines. Body:\n%s", got)
	}
}

func TestInstallHook_ZeroTimeoutEmitsLegacyLines(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-commit", InstallOpts{RegenChurn: true})
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	want := "(gortex enrich churn) >/dev/null 2>&1 || true"
	if !strings.Contains(got, want) {
		t.Errorf("zero timeout must emit legacy line %q. Body:\n%s", want, got)
	}
	if strings.Contains(got, "gortex_hook_run") {
		t.Errorf("zero timeout must not emit the watchdog helper. Body:\n%s", got)
	}
}

func TestInstallHook_NoActionsNoHelper(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-commit", InstallOpts{HookTimeoutSeconds: 30})
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "# (no regeneration actions enabled)") {
		t.Errorf("no-actions install should note it explicitly. Body:\n%s", got)
	}
	if strings.Contains(got, "gortex_hook_run") {
		t.Errorf("no actions means no hang surface — helper must not ship. Body:\n%s", got)
	}
}

func TestInstallHook_PostCheckoutUnchangedByTimeout(t *testing.T) {
	repo := initRepo(t)
	path, err := InstallHook(repo, "post-checkout", InstallOpts{HookTimeoutSeconds: 30})
	if err != nil {
		t.Fatalf("InstallHook: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read hook: %v", err)
	}
	got := string(body)
	if !strings.Contains(got, "touch .gortex/reindex.notify 2>/dev/null || true") {
		t.Errorf("post-checkout body must stay unchanged. Body:\n%s", got)
	}
	if strings.Contains(got, "gortex_hook_run") {
		t.Errorf("post-checkout has no gortex call — no helper expected. Body:\n%s", got)
	}
}

func TestHookPathFor_StaysInsideRepo(t *testing.T) {
	repo := initRepo(t)
	path, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	if !strings.HasPrefix(path, repo) {
		t.Errorf("hook path %q escapes the temp repo %q — a machine-global core.hooksPath would make every test write to the real global hooks dir", path, repo)
	}
}

func TestHookPathFor_HonoursCoreHooksPath(t *testing.T) {
	repo := initRepo(t)
	customHooks := filepath.Join(repo, "custom-hooks")
	if err := os.MkdirAll(customHooks, 0o755); err != nil {
		t.Fatalf("mkdir custom: %v", err)
	}
	cmd := exec.Command("git", "config", "core.hooksPath", customHooks)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("config core.hooksPath: %v: %s", err, out)
	}
	path, err := HookPathFor(repo, "post-commit")
	if err != nil {
		t.Fatalf("HookPathFor: %v", err)
	}
	if filepath.Dir(path) != customHooks {
		t.Errorf("HookPathFor should honour core.hooksPath, got %q under %q (want %q)",
			path, filepath.Dir(path), customHooks)
	}
}
