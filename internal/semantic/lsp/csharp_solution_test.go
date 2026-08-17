package lsp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSolutionFile(t *testing.T, root string, rel string) string {
	t.Helper()
	path := filepath.Join(root, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte("solution"), 0o644))
	return path
}

func TestIsCSharpLSCommand(t *testing.T) {
	assert.True(t, isCSharpLSCommand("csharp-ls"))
	assert.True(t, isCSharpLSCommand(filepath.Join("some", "tools", "csharp-ls")))
	assert.True(t, isCSharpLSCommand("csharp-ls.exe"))
	assert.False(t, isCSharpLSCommand("omnisharp"))
	assert.False(t, isCSharpLSCommand("jdtls"))
}

func TestCSharpSolutionForEnvWinsWhenInsideRoot(t *testing.T) {
	root := t.TempDir()
	writeSolutionFile(t, root, filepath.Join("projects", "All.slnx"))
	// Decoys: multiple root-level solutions must not matter when env resolves.
	writeSolutionFile(t, root, "A.sln")
	writeSolutionFile(t, root, "B.sln")

	t.Setenv(CSharpSolutionEnv, filepath.Join("projects", "All.slnx"))
	assert.Equal(t, filepath.Join("projects", "All.slnx"), csharpSolutionFor(root))

	// Absolute spelling of a solution inside the root is used as-is.
	abs := filepath.Join(root, "projects", "All.slnx")
	t.Setenv(CSharpSolutionEnv, abs)
	assert.Equal(t, abs, csharpSolutionFor(root))
}

func TestCSharpSolutionForEnvIgnoredOutsideRoot(t *testing.T) {
	// A daemon-global env value must be per-workspace safe: a path that does
	// not resolve inside THIS root is ignored and auto-detection takes over.
	root := t.TempDir()
	other := t.TempDir()
	writeSolutionFile(t, root, "Probe.sln")
	otherSln := writeSolutionFile(t, other, filepath.Join("projects", "All.slnx"))

	t.Setenv(CSharpSolutionEnv, filepath.Join("projects", "All.slnx"))
	assert.Equal(t, "Probe.sln", csharpSolutionFor(root),
		"missing relative env path must fall through to auto-detect")

	t.Setenv(CSharpSolutionEnv, otherSln)
	assert.Equal(t, "Probe.sln", csharpSolutionFor(root),
		"absolute env path outside the root must fall through to auto-detect")
}

func TestCSharpSolutionForAutoDetect(t *testing.T) {
	t.Setenv(CSharpSolutionEnv, "")

	single := t.TempDir()
	writeSolutionFile(t, single, "Only.sln")
	assert.Equal(t, "Only.sln", csharpSolutionFor(single))

	slnx := t.TempDir()
	writeSolutionFile(t, slnx, "Only.slnx")
	assert.Equal(t, "Only.slnx", csharpSolutionFor(slnx))

	multi := t.TempDir()
	writeSolutionFile(t, multi, "A.sln")
	writeSolutionFile(t, multi, "B.slnx")
	assert.Equal(t, "", csharpSolutionFor(multi),
		"several root solutions are ambiguous — no automatic pick")

	nested := t.TempDir()
	writeSolutionFile(t, nested, filepath.Join("sub", "Deep.sln"))
	assert.Equal(t, "", csharpSolutionFor(nested),
		"auto-detect scans the workspace root only")

	assert.Equal(t, "", csharpSolutionFor(t.TempDir()))
}

func TestCSharpSolutionArgs(t *testing.T) {
	root := t.TempDir()
	writeSolutionFile(t, root, "Only.sln")
	t.Setenv(CSharpSolutionEnv, "")

	assert.Equal(t, []string{"--solution", "Only.sln"}, csharpSolutionArgs(nil, root))
	assert.Equal(t, []string{"--stdio", "--solution", "Only.sln"},
		csharpSolutionArgs([]string{"--stdio"}, root))

	// A caller-supplied solution wins, in either spelling.
	explicit := []string{"--solution", "Custom.sln"}
	assert.Equal(t, explicit, csharpSolutionArgs(explicit, root))
	short := []string{"-s", "Custom.sln"}
	assert.Equal(t, short, csharpSolutionArgs(short, root))

	// No resolvable solution leaves the argv untouched.
	empty := t.TempDir()
	base := []string{"--stdio"}
	assert.Equal(t, base, csharpSolutionArgs(base, empty))
}

func TestCSharpRestoreArgsTargetSolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv(CSharpSolutionEnv, "")
	assert.Equal(t, []string{"restore", "-p:NuGetAudit=false"}, csharpRestoreArgs(root),
		"no solution keeps the bare directory restore")

	writeSolutionFile(t, root, filepath.Join("projects", "All.slnx"))
	writeSolutionFile(t, root, "A.sln") // multi-solution root: bare restore would fail MSB1011
	t.Setenv(CSharpSolutionEnv, filepath.Join("projects", "All.slnx"))
	assert.Equal(t,
		[]string{"restore", filepath.Join("projects", "All.slnx"), "-p:NuGetAudit=false"},
		csharpRestoreArgs(root),
		"a resolved solution becomes the restore target")
}
