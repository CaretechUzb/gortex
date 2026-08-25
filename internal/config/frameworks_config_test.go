package config

import (
	"os"
	"path/filepath"
	"testing"
)

// The load-bearing default: an absent index.frameworks block must allow
// every framework. Resolving it to "allow nothing" would silently strip
// every framework edge from the graph.
func TestFrameworksConfig_DefaultAllowsEverything(t *testing.T) {
	cfg := Default()
	set := cfg.Index.AllowedFrameworks()
	if set.Configured() {
		t.Errorf("Default() must not configure an allow-list, got %q", set.Patterns())
	}
	for _, name := range []string{"django", "odoo", "celery-dispatch"} {
		if !set.Allows(name) {
			t.Errorf("Default() must allow %q", name)
		}
	}
}

func TestFrameworksConfig_LoadFromYAML(t *testing.T) {
	dir := t.TempDir()
	body := "index:\n  frameworks:\n    allow:\n      - odoo\n      - godot*\n"
	if err := os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Join(dir, ".gortex.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(cfg.Index.Frameworks.Allow), 2; got != want {
		t.Fatalf("Allow len = %d, want %d (%q)", got, want, cfg.Index.Frameworks.Allow)
	}
	set := cfg.Index.AllowedFrameworks()
	if !set.Allows("odoo") {
		t.Error("odoo must be allowed")
	}
	if !set.Allows("godot-autoload") {
		t.Error("godot* must admit godot-autoload")
	}
	if set.Allows("django") {
		t.Error("django is not on the list and must be excluded")
	}
}

// A .gortex.yaml with no frameworks block must not disturb the rest of
// index:, and must allow everything.
func TestFrameworksConfig_AbsentBlockKeepsOtherIndexKeys(t *testing.T) {
	dir := t.TempDir()
	body := "index:\n  workers: 3\n"
	if err := os.WriteFile(filepath.Join(dir, ".gortex.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(filepath.Join(dir, ".gortex.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Index.Workers != 3 {
		t.Errorf("Workers = %d, want 3", cfg.Index.Workers)
	}
	if !cfg.Index.AllowedFrameworks().Allows("django") {
		t.Error("an absent frameworks block must allow every framework")
	}
}

// An empty list is indistinguishable from an absent key, so it must allow
// everything; running nothing is spelled with the `none` sentinel.
func TestFrameworksConfig_EmptyListAllowsEverything(t *testing.T) {
	cfg := IndexConfig{Frameworks: FrameworksConfig{Allow: []string{}}}
	if !cfg.AllowedFrameworks().Allows("django") {
		t.Error("an empty allow list must allow every framework")
	}
}

func TestFrameworksConfig_NoneSentinelAllowsNothing(t *testing.T) {
	cfg := IndexConfig{Frameworks: FrameworksConfig{Allow: []string{"none"}}}
	set := cfg.AllowedFrameworks()
	if set.Allows("django") || set.Allows("odoo") {
		t.Error("the none sentinel must exclude every framework")
	}
}

func TestFrameworksConfig_EnvOverrideReplacesList(t *testing.T) {
	cfg := IndexConfig{Frameworks: FrameworksConfig{Allow: []string{"django"}}}

	t.Setenv("GORTEX_FRAMEWORKS_ALLOW", "odoo, godot*")
	set := cfg.AllowedFrameworks()
	if set.Allows("django") {
		t.Error("the env override must REPLACE the configured list, not merge with it")
	}
	if !set.Allows("odoo") || !set.Allows("godot-autoload") {
		t.Errorf("env entries must apply, got %q", set.Patterns())
	}
}

// An explicitly empty variable means "allow everything", matching an
// absent key rather than narrowing to nothing.
func TestFrameworksConfig_EmptyEnvOverrideAllowsEverything(t *testing.T) {
	cfg := IndexConfig{Frameworks: FrameworksConfig{Allow: []string{"django"}}}
	t.Setenv("GORTEX_FRAMEWORKS_ALLOW", "")
	if !cfg.AllowedFrameworks().Allows("celery-dispatch") {
		t.Error("an empty GORTEX_FRAMEWORKS_ALLOW must allow every framework")
	}
}

func TestFrameworksConfig_UnsetEnvKeepsConfiguredList(t *testing.T) {
	os.Unsetenv("GORTEX_FRAMEWORKS_ALLOW")
	cfg := IndexConfig{Frameworks: FrameworksConfig{Allow: []string{"django"}}}
	set := cfg.AllowedFrameworks()
	if !set.Allows("django") || set.Allows("odoo") {
		t.Error("with the env unset the configured list must apply")
	}
}
