package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

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
