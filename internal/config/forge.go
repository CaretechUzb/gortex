package config

import "strings"

// ForgeConfig declares how a repository's git host maps onto a pull-request
// backend, for hosts the automatic routing cannot recognise.
//
// Routing normally works with no configuration: github.com and GitHub
// Enterprise resolve to the GitHub backend, and a host with a "gitlab" label or
// an existing `glab auth login` resolves to GitLab. This block exists for the
// cases that leaves out — a GitLab (or GHE) behind a vanity domain such as
// `code.internal.corp`, which carries nothing in its name to route on.
type ForgeConfig struct {
	// Hosts is a LIST, not a map keyed by hostname.
	//
	// That is forced by the loader, not a style choice: viper treats "." as a
	// key-path delimiter, so a map key of "code.internal.corp" is parsed as the
	// nested path code → internal → corp and the entry silently arrives under
	// the key "code". Every real hostname contains a dot, so the map form is
	// broken for exactly the hosts this block exists to serve.
	Hosts []ForgeHostConfig `mapstructure:"hosts" yaml:"hosts,omitempty"`
}

// ForgeHostConfig is one host's forge declaration.
type ForgeHostConfig struct {
	// Host is the git hostname exactly as it appears in the remote URL
	// (e.g. "code.internal.corp"). Matched case-insensitively.
	Host string `mapstructure:"host" yaml:"host,omitempty"`
	// Provider names the backend: "github" or "gitlab". Anything else is
	// ignored, so a typo degrades to automatic routing rather than to a host
	// that is silently addressed with the wrong API.
	Provider string `mapstructure:"provider" yaml:"provider,omitempty"`
	// APIBase overrides the REST base URL for this host — the GitLab v4 root
	// ("https://code.internal.corp/api/v4") or the GHE v3 root. Empty derives it
	// from the host.
	APIBase string `mapstructure:"api_base" yaml:"api_base,omitempty"`
	// TokenEnv names the environment variable holding this host's access token,
	// for an instance whose credential does not live in the standard
	// GITLAB_TOKEN / GH_TOKEN variables.
	//
	// It is deliberately the variable NAME, never the token itself: a config
	// file is committed far more often than it is protected, and a secret in
	// `.gortex.yaml` would be one `git add` away from the remote.
	TokenEnv string `mapstructure:"token_env" yaml:"token_env,omitempty"`
}

// ForgeHost returns the declared settings for host, matched case-insensitively.
// The second result reports whether a declaration exists.
func (c *ForgeConfig) ForgeHost(host string) (ForgeHostConfig, bool) {
	if c == nil || len(c.Hosts) == 0 {
		return ForgeHostConfig{}, false
	}
	want := strings.ToLower(strings.TrimSpace(host))
	if want == "" {
		return ForgeHostConfig{}, false
	}
	for _, entry := range c.Hosts {
		if strings.ToLower(strings.TrimSpace(entry.Host)) == want {
			return entry, true
		}
	}
	return ForgeHostConfig{}, false
}
