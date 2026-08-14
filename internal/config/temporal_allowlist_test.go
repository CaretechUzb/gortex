package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeTemporalAllowlist(t *testing.T, root, content string) {
	t.Helper()
	dir := filepath.Join(root, ".gortex")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "temporal-allowlist.yaml"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadLocalTemporalEnvHelpersGateAndNormalization(t *testing.T) {
	root := t.TempDir()
	writeTemporalAllowlist(t, root, "env_helpers:\n  - ' HelperA '\n  - ''\n  - HelperB\n")

	t.Setenv(LocalTemporalOptInEnv, "")
	if got := LoadLocalTemporalEnvHelpers(root); got != nil {
		t.Fatalf("gate off returned %#v", got)
	}

	for _, truthy := range []string{"1", " 1 ", "true", " TrUe "} {
		t.Run(truthy, func(t *testing.T) {
			t.Setenv(LocalTemporalOptInEnv, truthy)
			want := []string{"HelperA", "HelperB"}
			if got := LoadLocalTemporalEnvHelpers(root); !reflect.DeepEqual(got, want) {
				t.Fatalf("got %#v, want %#v", got, want)
			}
		})
	}
}

func TestLoadLocalTemporalEnvHelpersFailSoft(t *testing.T) {
	t.Setenv(LocalTemporalOptInEnv, "true")
	if got := LoadLocalTemporalEnvHelpers(t.TempDir()); got != nil {
		t.Fatalf("missing file returned %#v", got)
	}

	root := t.TempDir()
	writeTemporalAllowlist(t, root, "env_helpers: [")
	if got := LoadLocalTemporalEnvHelpers(root); got != nil {
		t.Fatalf("malformed file returned %#v", got)
	}
}
