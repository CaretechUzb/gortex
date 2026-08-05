package main

import (
	"bytes"
	"os/exec"
	"strings"

	"github.com/zzet/gortex/internal/churn"
)

// gitCommitHash returns the HEAD commit hash for the repository at dir,
// or an empty string if git is unavailable or the directory is not a repo.
func gitCommitHash(dir string) string {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(out.String())
}

// gitBranch returns the current branch name for the repository at dir.
// It returns an empty string when git is unavailable, the directory is
// not a repo, or HEAD is detached.
func gitBranch(dir string) string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	cmd.Dir = dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}
	branch := strings.TrimSpace(out.String())
	if branch == "HEAD" {
		return "" // detached HEAD — no branch to key on
	}
	return branch
}

// gitDefaultBranch returns the repository's default branch as a
// rev-parseable reference. Thin wrapper over churn.DefaultBranch so
// the CLI, daemon controller, and MCP tool resolve the same branch
// the same way.
func gitDefaultBranch(dir string) string {
	return churn.DefaultBranch(dir)
}
