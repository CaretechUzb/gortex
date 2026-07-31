package mcp

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/persistence"
)

// closeSidecarOnCleanup releases the sidecar rooted at dataDir before the
// enclosing t.TempDir() is removed.
//
// OpenSidecar keeps handles in a process-wide cache keyed by absolute path, so
// any manager built over a test directory leaves that file open for the life
// of the test binary. POSIX unlinks an open file happily and nothing shows;
// Windows refuses, t.TempDir()'s RemoveAll fails, and the test is reported as
// failed even though every assertion in it passed.
//
// Cleanups run LIFO, so registering here — after t.TempDir() has registered
// its own removal — closes the handle first.
func closeSidecarOnCleanup(t *testing.T, dataDir string) {
	t.Helper()
	t.Cleanup(func() {
		require.NoError(t, persistence.CloseSidecar(persistence.DefaultSidecarPath(dataDir)))
	})
}

// tempSidecarDir is the common case: a temp directory used directly as a
// manager's data dir.
func tempSidecarDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	closeSidecarOnCleanup(t, dir)
	return dir
}

// tempNotebookRepo returns a temp repo directory for the notebook manager,
// which roots its sidecar at <repo>/.gortex rather than at the path it is
// handed — so the release has to name that subdirectory.
func tempNotebookRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	closeSidecarOnCleanup(t, filepath.Join(dir, ".gortex"))
	return dir
}
