package mcp

// Regression cover for issue #297: in a daemon tracking several repos, the
// LSP workspace (which becomes the language server's working directory) was
// derived from the daemon process's own launch cwd rather than from the
// tracked repo's path, producing a directory that exists nowhere and an
// opaque `chdir ...` spawn failure.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWorkspaceRootFor_UsesTrackedRepoRoot — a file inside a tracked repo
// must resolve to that repo's recorded root, whatever directory the process
// is running from.
func TestWorkspaceRootFor_UsesTrackedRepoRoot(t *testing.T) {
	fx := newSharedWorkspaceServer(t, false)

	for _, repo := range []string{fx.repoA, fx.repoB} {
		root, err := fx.srv.workspaceRootFor(filepath.Join(repo, "main.go"))
		require.NoError(t, err)
		require.Equal(t, repo, root, "workspace must be the tracked repo root")
	}
}

// TestWorkspaceRootFor_RejectsPathUnderNoTrackedRepo — the reported failure
// shape. A path synthesised by joining a repo-relative fragment onto the
// daemon's launch cwd belongs to no tracked repo and names no real
// directory; it must be refused rather than handed to a spawn.
func TestWorkspaceRootFor_RejectsPathUnderNoTrackedRepo(t *testing.T) {
	fx := newSharedWorkspaceServer(t, false)

	phantom := filepath.Join(t.TempDir(), "daemon-launch-cwd", "SomeProject", "SomeFolder", "Thing.cs")
	_, err := fx.srv.workspaceRootFor(phantom)
	require.Error(t, err, "a workspace under no tracked repo must not be invented")
	require.Contains(t, err.Error(), phantom)
}

// TestWorkspaceRootFor_AllowsRealUntrackedDirectory — an existing directory
// outside every tracked repo (a scratch file an editor opened) is still a
// legitimate workspace. Only phantom directories are refused.
func TestWorkspaceRootFor_AllowsRealUntrackedDirectory(t *testing.T) {
	fx := newSharedWorkspaceServer(t, false)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "scratch.go"), []byte("package x\n"), 0o644))

	root, err := fx.srv.workspaceRootFor(filepath.Join(dir, "scratch.go"))
	require.NoError(t, err)
	require.Equal(t, dir, root)
}

// TestWorkspaceRootFor_RespectsPathComponentBoundaries — the root /src/repo
// must not claim a file under a sibling /src/repo-old. A plain string-prefix
// containment check used to.
func TestWorkspaceRootFor_RespectsPathComponentBoundaries(t *testing.T) {
	fx := newSharedWorkspaceServer(t, false)

	sibling := fx.repoB + "-old"
	require.NoError(t, os.MkdirAll(sibling, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(sibling, "main.go"), []byte("package main\n"), 0o644))

	root, err := fx.srv.workspaceRootFor(filepath.Join(sibling, "main.go"))
	require.NoError(t, err)
	require.Equal(t, sibling, root, "a name-prefix sibling must not be folded into the tracked repo")
}

// TestAbsolutePath_AnchorsUnprefixedPathOnTrackedRepo — a repo-relative
// fragment that carries no repo prefix must be anchored on a tracked repo,
// not joined onto the daemon's own working directory.
func TestAbsolutePath_AnchorsUnprefixedPathOnTrackedRepo(t *testing.T) {
	fx := newSharedWorkspaceServer(t, false)

	// A file that exists in exactly one tracked repo, under a nested path —
	// the shape the bug report redacted as "SomeProject\SomeFolder".
	nested := filepath.Join(fx.repoB, "SomeProject", "SomeFolder")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "Thing.cs"), []byte("class Thing {}\n"), 0o644))

	got, err := fx.srv.absolutePath(filepath.Join("SomeProject", "SomeFolder", "Thing.cs"))
	require.NoError(t, err)
	require.Equal(t, filepath.Join(nested, "Thing.cs"), got)

	// And the workspace derived from it is repoB's root — the whole point.
	root, err := fx.srv.workspaceRootFor(got)
	require.NoError(t, err)
	require.Equal(t, fx.repoB, root)
}

// TestAbsolutePath_UnresolvableStaysOutOfTrackedRepos — a fragment that
// matches nothing on disk still yields something absolute for error
// reporting, but it must not be presented as belonging to a tracked repo.
func TestAbsolutePath_UnresolvableStaysOutOfTrackedRepos(t *testing.T) {
	fx := newSharedWorkspaceServer(t, false)

	got, err := fx.srv.absolutePath(filepath.Join("NoSuchProject", "NoSuchFolder", "Nope.cs"))
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(got))

	// The downstream workspace derivation is what must refuse it.
	_, werr := fx.srv.workspaceRootFor(got)
	require.Error(t, werr)
}
