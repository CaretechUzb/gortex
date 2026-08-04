package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIndexGeneratedParsersDefaultsOffAndDecodes(t *testing.T) {
	if Default().Index.IndexGeneratedParsers {
		t.Fatal("index.index_generated_parsers must default to false")
	}

	path := filepath.Join(t.TempDir(), ".gortex.yaml")
	if err := os.WriteFile(path, []byte("index:\n  index_generated_parsers: true\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Index.IndexGeneratedParsers {
		t.Fatal("index.index_generated_parsers did not decode")
	}
}
