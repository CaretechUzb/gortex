package persistence

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CloseSidecar exists so a test can release the process-wide handle cache
// before its temp directory is removed. On POSIX an open file unlinks happily,
// so nothing here would fail even if CloseSidecar were a no-op — the case that
// actually bites is Windows, where RemoveAll refuses while a handle is open.
// This test is therefore wired into the Windows CI job as well.
func TestCloseSidecar_ReleasesTheHandleAndStaysHarmless(t *testing.T) {
	dir := t.TempDir()
	path := DefaultSidecarPath(dir)

	t.Run("closing a path that was never opened creates nothing", func(t *testing.T) {
		unopened := filepath.Join(t.TempDir(), "nested", "sidecar.sqlite")
		require.NoError(t, CloseSidecar(unopened))

		_, err := os.Stat(unopened)
		assert.True(t, os.IsNotExist(err), "no database file may be created")
		_, err = os.Stat(filepath.Dir(unopened))
		assert.True(t, os.IsNotExist(err), "no parent directory may be created")
	})

	t.Run("empty path is a no-op", func(t *testing.T) {
		require.NoError(t, CloseSidecar(""))
	})

	first, err := OpenSidecar(path)
	require.NoError(t, err)
	require.NotNil(t, first)

	t.Run("reopen after close yields a usable store", func(t *testing.T) {
		require.NoError(t, CloseSidecar(path))

		second, err := OpenSidecar(path)
		require.NoError(t, err)
		require.NotNil(t, second)
		assert.NotSame(t, first, second,
			"the cache entry must be gone, so a reopen builds a fresh store")

		// Usable, not merely non-nil: the reopened handle has to serve a query.
		var one int
		require.NoError(t, second.db.QueryRow("SELECT 1").Scan(&one))
		assert.Equal(t, 1, one)

		require.NoError(t, CloseSidecar(path))
	})

	t.Run("closing repeatedly is harmless", func(t *testing.T) {
		require.NoError(t, CloseSidecar(path))
		require.NoError(t, CloseSidecar(path))
	})

	t.Run("the containing directory can be removed afterwards", func(t *testing.T) {
		// The point of the whole exercise. RemoveAll succeeds on POSIX whether
		// or not the handle was released, so this assertion only has teeth on
		// a platform that refuses to unlink an open file.
		if runtime.GOOS != "windows" {
			t.Log("note: on this platform RemoveAll would succeed even with the handle open")
		}
		require.NoError(t, os.RemoveAll(dir),
			"a released sidecar must not keep its directory pinned")
		_, err := os.Stat(dir)
		assert.True(t, os.IsNotExist(err))
	})
}
