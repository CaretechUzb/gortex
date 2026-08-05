package serverstack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
)

// TestNewSharedServer_Oneshot asserts the shared constructor builds a
// working stack over a tmp repo: the graph indexes, and the engine /
// MCP server / overlay manager are wired. This is the single-construction-
// path validation.
func TestNewSharedServer_Oneshot(t *testing.T) {
	repo := t.TempDir()
	src := "package toy\n\nfunc Add(a, b int) int { return a + b }\nfunc Mul(a, b int) int { return a * b }\n"
	if err := os.WriteFile(filepath.Join(repo, "toy.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	ss, err := NewSharedServer(SharedServerConfig{
		Lifecycle: LifecycleOneshot,
		Index:     repo,
		// Private per-process store: never the shared default.
		BackendPath: filepath.Join(t.TempDir(), "embedded.sqlite"),
		Config:      config.Default(),
		Logger:      zap.NewNop(),
		Version:     "test",
		SideStores:  SideStores{NotesDir: t.TempDir(), NotesRepo: "test"},
		// Pin the savings ledger + legacy-import probe to temp paths:
		// with both empty the constructor opens the REAL machine-global
		// sidecar and imports (renaming!) the developer's real flat-file
		// ledger — a unit test must never mutate ~/.gortex.
		SavingsPath:       filepath.Join(t.TempDir(), "sidecar.sqlite"),
		SavingsLegacyJSON: filepath.Join(t.TempDir(), "savings.json"),
	})
	if err != nil {
		t.Fatalf("NewSharedServer: %v", err)
	}
	defer ss.Close()

	if ss.Graph == nil || ss.Indexer == nil || ss.Engine == nil || ss.MCP == nil {
		t.Fatalf("incomplete stack: graph=%v idx=%v eng=%v mcp=%v", ss.Graph != nil, ss.Indexer != nil, ss.Engine != nil, ss.MCP != nil)
	}
	if ss.Overlays == nil {
		t.Error("overlay manager should be wired")
	}

	if _, err := ss.Indexer.Index(repo); err != nil {
		t.Fatalf("Index: %v", err)
	}
	if n := ss.Graph.Stats().TotalNodes; n == 0 {
		t.Fatal("graph should be non-empty after indexing the tmp repo")
	}
}

// TestNewSharedServer_LifecyclesDefaultToSqlite asserts every lifecycle
// resolves to the sqlite backend when Backend is empty, and that one-shot
// alone stays outside the store lock.
func TestNewSharedServer_LifecyclesDefaultToSqlite(t *testing.T) {
	for _, l := range []Lifecycle{LifecycleDaemon, LifecycleHTTP, LifecycleOneshot} {
		if got := l.defaultBackend(); got != "sqlite" {
			t.Errorf("lifecycle %d default backend = %q, want sqlite", l, got)
		}
	}
	if LifecycleOneshot.Writable() {
		t.Error("oneshot must not be writable (no store lock)")
	}
	if !LifecycleDaemon.Writable() || !LifecycleHTTP.Writable() {
		t.Error("daemon/http lifecycles own a durable store")
	}
}

// TestNewSharedServer_OneshotRefusesSharedStore pins the safety property
// that keeps the unlocked embedded server off the daemon's database: with
// no BackendPath the sqlite path would resolve to ~/.gortex/store, so the
// constructor must refuse instead of opening it.
func TestNewSharedServer_OneshotRefusesSharedStore(t *testing.T) {
	_, err := NewSharedServer(SharedServerConfig{
		Lifecycle: LifecycleOneshot,
		Index:     t.TempDir(),
		Config:    config.Default(),
		Logger:    zap.NewNop(),
	})
	if err == nil {
		t.Fatal("one-shot without BackendPath must be refused, not pointed at the shared store")
	}
	if !strings.Contains(err.Error(), "BackendPath") {
		t.Errorf("error should name the missing BackendPath, got: %v", err)
	}
}
