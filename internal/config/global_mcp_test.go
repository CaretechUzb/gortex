package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadGlobalMCPAllowEmbedded(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{name: "block absent", yaml: "repos: []\n", want: false},
		{name: "explicitly disabled", yaml: "mcp:\n  allow_embedded: false\n", want: false},
		{name: "explicitly enabled", yaml: "mcp:\n  allow_embedded: true\n", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			require.NoError(t, os.WriteFile(path, []byte(tt.yaml), 0o600))

			cfg, err := LoadGlobal(path)
			require.NoError(t, err)
			require.Equal(t, tt.want, cfg.MCP.AllowEmbedded)
		})
	}
}

func TestGlobalMCPAllowEmbeddedRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &GlobalConfig{MCP: GlobalMCPConfig{AllowEmbedded: true}}
	cfg.SetConfigPath(path)

	require.NoError(t, cfg.Save())
	reloaded, err := LoadGlobal(path)
	require.NoError(t, err)
	require.True(t, reloaded.MCP.AllowEmbedded)
}

func TestUnknownGlobalKeysRecognizesMCP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	data := []byte("mcp:\n  allow_embedded: true\ntypo: true\n")
	require.NoError(t, os.WriteFile(path, data, 0o600))

	require.Equal(t, []string{"typo"}, UnknownGlobalKeys(path))
}
