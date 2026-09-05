package config

import (
	"os"
	"testing"
)

func TestForgeHost_Lookup(t *testing.T) {
	cfg := &ForgeConfig{Hosts: []ForgeHostConfig{
		{Host: "Code.Internal.Corp", Provider: "gitlab", APIBase: "https://code.internal.corp/api/v4", TokenEnv: "CORP_TOKEN"},
	}}

	got, ok := cfg.ForgeHost("code.internal.corp")
	if !ok {
		t.Fatal("lookup missed on a case-different host")
	}
	if got.Provider != "gitlab" || got.TokenEnv != "CORP_TOKEN" {
		t.Errorf("entry = %+v", got)
	}
	if _, ok := cfg.ForgeHost("  CODE.INTERNAL.CORP  "); !ok {
		t.Error("lookup missed on a padded host")
	}
	if _, ok := cfg.ForgeHost("other.example.com"); ok {
		t.Error("lookup hit on an undeclared host")
	}
	if _, ok := cfg.ForgeHost(""); ok {
		t.Error("lookup hit on an empty host")
	}
	// A nil receiver and an empty table are both safe.
	var nilCfg *ForgeConfig
	if _, ok := nilCfg.ForgeHost("x"); ok {
		t.Error("nil ForgeConfig returned a hit")
	}
	if _, ok := (&ForgeConfig{}).ForgeHost("x"); ok {
		t.Error("empty ForgeConfig returned a hit")
	}
}

// TestForgeConfig_ParsesFromYAML pins the wire shape operators actually write.
func TestForgeConfig_ParsesFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/.gortex.yaml"
	body := `
forge:
  hosts:
    - host: code.internal.corp
      provider: gitlab
      api_base: https://code.internal.corp/gitlab/api/v4
      token_env: CORP_GITLAB_TOKEN
`
	if err := writeFileForTest(path, body); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	entry, ok := cfg.Forge.ForgeHost("code.internal.corp")
	if !ok {
		t.Fatalf("forge.hosts did not parse: %+v", cfg.Forge)
	}
	if entry.Provider != "gitlab" {
		t.Errorf("provider = %q", entry.Provider)
	}
	if entry.APIBase != "https://code.internal.corp/gitlab/api/v4" {
		t.Errorf("api_base = %q", entry.APIBase)
	}
	if entry.TokenEnv != "CORP_GITLAB_TOKEN" {
		t.Errorf("token_env = %q", entry.TokenEnv)
	}
}

func writeFileForTest(path, body string) error {
	return os.WriteFile(path, []byte(body), 0o600)
}
