package serverstack

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/indexer"
)

func TestLifecycleSharedServerCloseRejectsExtractionAndMutations(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "toy.go"), []byte("package toy\n\nfunc Toy() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ss, err := NewSharedServer(SharedServerConfig{
		Lifecycle:         LifecycleOneshot,
		Index:             repo,
		BackendPath:       filepath.Join(t.TempDir(), "embedded.sqlite"),
		Config:            config.Default(),
		Logger:            zap.NewNop(),
		Version:           "test",
		SideStores:        SideStores{NotesDir: t.TempDir(), NotesRepo: "test"},
		SavingsPath:       filepath.Join(t.TempDir(), "sidecar.sqlite"),
		SavingsLegacyJSON: filepath.Join(t.TempDir(), "savings.json"),
	})
	if err != nil {
		t.Fatalf("NewSharedServer: %v", err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = ss.Close()
		}
	})
	if ss.MultiIndexer == nil {
		t.Fatal("shared server did not construct a MultiIndexer")
	}

	err = ss.Close()
	closed = true
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := ss.Indexer.ExtractBuffer("go", "after.go", []byte("package after\n")); !errors.Is(err, indexer.ErrIndexerClosed) {
		t.Fatalf("post-close standalone extraction error = %v, want ErrIndexerClosed", err)
	}

	afterRepo := t.TempDir()
	if err := os.WriteFile(filepath.Join(afterRepo, "after.go"), []byte("package after\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ss.MultiIndexer.TrackRepo(config.RepoEntry{Path: afterRepo, Name: "after"}); err == nil || !strings.Contains(strings.ToLower(err.Error()), "closed") {
		t.Fatalf("post-close mutation error = %v, want closed lifecycle", err)
	}
}
