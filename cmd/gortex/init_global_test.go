package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/config"
)

func TestDefaultGlobalConfigStubDisablesEmbeddedMCP(t *testing.T) {
	const embeddedPolicy = "\nmcp:\n  allow_embedded: false\n"
	if !strings.Contains(defaultGlobalConfigStub, embeddedPolicy) {
		t.Fatalf("default global config must expose the disabled embedded MCP policy")
	}

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(defaultGlobalConfigStub), 0o600); err != nil {
		t.Fatalf("write default global config: %v", err)
	}
	global, err := config.LoadGlobal(path)
	if err != nil {
		t.Fatalf("load default global config: %v", err)
	}
	if global.MCP.AllowEmbedded {
		t.Fatal("embedded MCP must be disabled in the default global config")
	}
}
