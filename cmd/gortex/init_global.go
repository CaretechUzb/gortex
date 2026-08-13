package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/daemon"
)

// This file hosts daemon-side helpers shared between `gortex install`
// (which spawns / tracks at user-level setup time) and `gortex init`
// (which uses ensureGlobalConfig to register the repo with the
// daemon config). They don't fit the Adapter interface because they
// touch the daemon's RPC protocol, not on-disk agent config.

// ensureGlobalConfigExists creates an empty ~/.gortex/config.yaml
// when none is present. The daemon needs a writable path on first
// Track; creating it now surfaces any permission problems at install
// time instead of on the first use.
func ensureGlobalConfigExists() error {
	path := config.DefaultGlobalConfigPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("ensure config dir: %w", err)
	}
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(defaultGlobalConfigStub), 0o600)
}

// defaultGlobalConfigStub is the on-disk shape of a fresh global
// config. It documents the skip-embedding defaults inline so users
// don't have to dig through source to know what's being skipped.
const defaultGlobalConfigStub = `active_project: ""
repos: []
projects: {}

# Global ignore list. Layered under builtin (always applies) and above
# per-repo entries and workspace .gortex.yaml. Gitignore semantics;
# use "!pattern" in a later layer to re-include.
exclude: []

# MCP client startup policy. Keep false to require the shared daemon.
mcp:
  allow_embedded: false

# Semantic search tuning.
semantic:
  # Node (language, kind) pairs skipped during vector-index construction.
  # They stay queryable by name/kind/filepath — only semantic search is
  # turned off. Reclaim ~hundreds of MiB on monorepos heavy in CSS
  # tokens or terraform resources.
  skip_embed:
    - language: css
      kinds: [variable, type]   # custom properties, class/id selectors
    - language: hcl
      kinds: [type, variable]   # terraform resources, locals, variables
    - language: yaml
      kinds: [variable]         # yaml keys
    - language: toml
      kinds: [variable]         # toml keys
    - language: bash
      kinds: [variable]         # shell variables
`

// trackViaDaemon opens a control-mode client and issues a Track for
// the given absolute path. Returns a human-readable status for
// display.
func trackViaDaemon(absPath string) (string, error) {
	c, err := daemon.Dial(daemon.Handshake{Mode: daemon.ModeControl, ClientName: "cli"})
	if err != nil {
		return "", err
	}
	defer c.Close()
	resp, err := c.Control(daemon.ControlTrack, daemon.TrackParams{Path: absPath})
	if err != nil {
		return "", err
	}
	if !resp.OK {
		return "", fmt.Errorf("%s: %s", resp.ErrorCode, resp.ErrorMsg)
	}
	return string(resp.Result), nil
}
