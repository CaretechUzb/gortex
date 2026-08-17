package lsp

import (
	"os"
	"path/filepath"
	"strings"
)

// CSharpSolutionEnv names the solution file csharp-ls should load and the C#
// pre-restore should target — a PATH-style list of candidates (relative to
// the workspace root, or absolute inside it), each root using the first
// entry that resolves inside it. The variable is daemon-global while
// solutions are per-workspace, so per-root resolution is what makes one
// setting serve a multi-repo daemon: entries that do not resolve inside a
// given root are ignored for that root, and several repos with different
// solutions each pick their own entry. Empty (the default), or no entry
// resolving, auto-detects: a workspace root carrying exactly one .sln/.slnx
// uses it; anything else keeps the server's own discovery.
const CSharpSolutionEnv = "GORTEX_LSP_CSHARP_SOLUTION"

// isCSharpLSCommand reports whether a resolved LSP command is csharp-ls,
// whether configured bare, as a path, or with a Windows .exe suffix.
func isCSharpLSCommand(command string) bool {
	return strings.TrimSuffix(filepath.Base(command), ".exe") == "csharp-ls"
}

// csharpSolutionFor resolves the solution file for a workspace root: the
// CSharpSolutionEnv value when it names a file inside the root, else the
// single root-level .sln/.slnx when exactly one exists, else "". The returned
// spelling is what the argv carries — the env value as given, or the bare
// file name for an auto-detected root solution; both resolve against the
// server's working directory, which is the workspace root.
func csharpSolutionFor(workspaceRoot string) string {
	for _, entry := range strings.Split(os.Getenv(CSharpSolutionEnv), string(os.PathListSeparator)) {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		abs := entry
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(workspaceRoot, entry)
		}
		if pathInsideRoot(workspaceRoot, abs) && solutionFileExists(abs) {
			return entry
		}
	}
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return ""
	}
	found := ""
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch strings.ToLower(filepath.Ext(e.Name())) {
		case ".sln", ".slnx":
			if found != "" {
				return "" // several root solutions are ambiguous — no pick
			}
			found = e.Name()
		}
	}
	return found
}

// csharpSolutionArgs appends `--solution <file>` to a csharp-ls launch argv
// when a solution resolves for the workspace root and the caller has not
// already pinned one (either spelling). csharp-ls's own discovery predates
// .slnx and has no answer for a multi-solution root, where the workspace
// loads nothing.
func csharpSolutionArgs(baseArgs []string, workspaceRoot string) []string {
	for _, a := range baseArgs {
		if a == "--solution" || a == "-s" {
			return baseArgs
		}
	}
	sln := csharpSolutionFor(workspaceRoot)
	if sln == "" {
		return baseArgs
	}
	out := append([]string(nil), baseArgs...)
	return append(out, "--solution", sln)
}

// csharpRestoreArgs builds the pre-spawn `dotnet` argv: targeted at the
// resolved solution when one exists, else the bare directory restore. A bare
// restore in a multi-solution root fails with MSB1011 ("more than one project
// or solution file"), so the audit-suppressed assets the workspace load needs
// are never written — targeting the same solution the server loads closes
// that hole.
func csharpRestoreArgs(workspaceRoot string) []string {
	if sln := csharpSolutionFor(workspaceRoot); sln != "" {
		return []string{"restore", sln, "-p:NuGetAudit=false"}
	}
	return []string{"restore", "-p:NuGetAudit=false"}
}

// pathInsideRoot reports whether path (absolute) lies under root.
func pathInsideRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// solutionFileExists reports whether path names an existing regular file.
func solutionFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
