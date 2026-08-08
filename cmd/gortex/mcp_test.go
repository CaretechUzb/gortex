package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/platform"
)

// TestEmbeddedStorePathIsPrivateTemp pins the property that keeps the
// unlocked embedded MCP server off the daemon's database: it must run
// against a non-empty path inside a temp directory it owns, never the
// shared default store an empty path would resolve to.
func TestOneshotEmbeddedStorePathIsPrivateTemp(t *testing.T) {
	path, remove, err := newEmbeddedStorePath()
	if err != nil {
		t.Fatalf("newEmbeddedStorePath: %v", err)
	}
	defer remove()

	if strings.TrimSpace(path) == "" {
		t.Fatal("store path must never be empty — an empty path resolves to the shared default store")
	}
	if !filepath.IsAbs(path) {
		t.Errorf("store path should be absolute, got %q", path)
	}

	dir := filepath.Dir(path)
	tempRoot, err := filepath.EvalSymlinks(os.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	resolvedDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve store dir: %v", err)
	}
	if rel, rerr := filepath.Rel(tempRoot, resolvedDir); rerr != nil || strings.HasPrefix(rel, "..") {
		t.Errorf("store dir %q is not under the temp root %q", resolvedDir, tempRoot)
	}

	if shared := platform.StoreDir(); shared != "" {
		if rel, rerr := filepath.Rel(shared, dir); rerr == nil && !strings.HasPrefix(rel, "..") {
			t.Errorf("store path %q lives under the shared store dir %q", path, shared)
		}
	}

	if _, serr := os.Stat(dir); serr != nil {
		t.Fatalf("store dir should exist before cleanup: %v", serr)
	}
	remove()
	if _, serr := os.Stat(dir); !os.IsNotExist(serr) {
		t.Errorf("cleanup should remove the store dir, stat err = %v", serr)
	}
}

// TestReapStaleEmbeddedStores pins the two halves of the reaper: a store
// directory a killed process left behind is removed once it ages past the
// TTL, and a fresh one (the store a concurrent server is using) is not.
func TestReapStaleEmbeddedStores(t *testing.T) {
	tmp := redirectTempRoot(t)

	stale := filepath.Join(tmp, "gortex-mcp-store-stale")
	fresh := filepath.Join(tmp, "gortex-mcp-store-fresh")
	unrelated := filepath.Join(tmp, "some-other-tempdir")
	for _, dir := range []string{stale, fresh, unrelated} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	ageDirBeyondTTL(t, stale)
	ageDirBeyondTTL(t, unrelated)

	reapStaleEmbeddedStores(zap.NewNop())

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale store dir survived the reap, stat err = %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh store dir must be left alone, stat err = %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Errorf("reaper touched a directory that is not an embedded store, stat err = %v", err)
	}
}

// TestReapStaleEmbeddedStoresSpareLockedStore is the liveness contract. A
// `gortex mcp` session that has been serving for longer than the TTL still
// owns its store: sqlite writes land in files INSIDE the directory and never
// bump the directory's own mtime, so age alone reports a busy server's store
// as abandoned and a newer launch deletes the database out from under it.
// The advisory lock the live store holds is the real proof of occupancy, and
// only an aged directory whose lock is free may be removed.
func TestReapStaleEmbeddedStoresSpareLockedStore(t *testing.T) {
	tmp := redirectTempRoot(t)

	// The live store: allocated exactly as a running embedded server
	// allocates it, so it holds whatever lock the production path takes.
	livePath, releaseLive, err := newEmbeddedStorePath()
	if err != nil {
		t.Fatalf("newEmbeddedStorePath: %v", err)
	}
	defer releaseLive()
	if err := os.WriteFile(livePath, []byte("live sqlite bytes"), 0o600); err != nil {
		t.Fatalf("write live store file: %v", err)
	}
	liveDir := filepath.Dir(livePath)
	// The directory looks ancient — the whole point is that its age says
	// nothing about whether the process using it is alive.
	ageDirBeyondTTL(t, liveDir)

	// The abandoned store: same shape, same age, but its owner is gone, so
	// nothing holds the lock file it left behind.
	abandonedDir := filepath.Join(tmp, "gortex-mcp-store-abandoned")
	if err := os.MkdirAll(abandonedDir, 0o755); err != nil {
		t.Fatalf("mkdir abandoned dir: %v", err)
	}
	for _, name := range []string{"embedded.sqlite", embeddedStoreLockName} {
		if err := os.WriteFile(filepath.Join(abandonedDir, name), []byte("orphaned"), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	ageDirBeyondTTL(t, abandonedDir)

	reapStaleEmbeddedStores(zap.NewNop())

	if _, err := os.Stat(liveDir); err != nil {
		t.Errorf("the reaper deleted a live embedded store dir, stat err = %v", err)
	}
	if _, err := os.Stat(livePath); err != nil {
		t.Errorf("the reaper deleted a live embedded store file, stat err = %v", err)
	}
	if _, err := os.Stat(abandonedDir); !os.IsNotExist(err) {
		t.Errorf("an aged unlocked store dir survived the reap, stat err = %v", err)
	}
}

// redirectTempRoot points os.TempDir at a per-test directory so the reaper
// only ever sees this test's fixtures, and skips when the platform will not
// honour the redirect.
func redirectTempRoot(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	// os.TempDir reads TMPDIR on unix and TMP/TEMP on Windows.
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	if os.TempDir() != tmp {
		t.Skipf("temp root not redirectable on this platform: %q", os.TempDir())
	}
	return tmp
}

// ageDirBeyondTTL backdates dir's mtime past the reaper's age pre-filter.
func ageDirBeyondTTL(t *testing.T, dir string) {
	t.Helper()
	aged := time.Now().Add(-staleEmbeddedStoreTTL - time.Hour)
	if err := os.Chtimes(dir, aged, aged); err != nil {
		t.Fatalf("chtimes %s: %v", dir, err)
	}
}
