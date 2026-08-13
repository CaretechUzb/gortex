package codex

import (
	"os"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

func TestCodexPreToolUseMigrationPreservesCoLocatedUserHandlers(t *testing.T) {
	managed := `
[[hooks.PreToolUse.hooks]]
type = "command"
command = "` + testCodexHookCommand + `"
statusMessage = "Old Gortex PreToolUse"
`
	user := `
[[hooks.PreToolUse.hooks]]
type = "command"
command = "echo colocated-user-pretooluse"
statusMessage = "User PreToolUse"
`
	for _, tt := range []struct {
		name     string
		handlers string
		force    bool
	}{
		{name: "managed first", handlers: managed + user},
		{name: "user first", handlers: user + managed},
		{name: "force", handlers: managed + user, force: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := codexGlobalEnv(t)
			path := codexConfigPath(env)
			seed := `[[hooks.PreToolUse]]
matcher = "^Bash$"
` + tt.handlers
			if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
				t.Fatalf("seed config: %v", err)
			}

			if _, err := InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{Force: tt.force}); err != nil {
				t.Fatalf("migrate mixed PreToolUse group: %v", err)
			}
			cfg := readCodexConfig(t, env)
			if count := hookMatcherCommandCount(t, cfg, "PreToolUse", codexLegacyBashPreToolUseMatcher, "echo colocated-user-pretooluse"); count != 1 {
				t.Fatalf("co-located user handler count=%d want 1: %#v", count, preToolUseEntries(t, cfg))
			}
			if count := hookMatcherCommandCount(t, cfg, "PreToolUse", codexLegacyBashPreToolUseMatcher, testCodexHookCommand); count != 0 {
				t.Fatalf("legacy Gortex handler survived mixed-group migration: %#v", preToolUseEntries(t, cfg))
			}
			assertGortexPreToolUseHooks(t, cfg)

			if _, err := InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{}); err != nil {
				t.Fatalf("reapply hooks: %v", err)
			}
			cfg = readCodexConfig(t, env)
			if count := hookMatcherCommandCount(t, cfg, "PreToolUse", codexLegacyBashPreToolUseMatcher, "echo colocated-user-pretooluse"); count != 1 {
				t.Fatalf("reapply changed co-located user handler count=%d: %#v", count, preToolUseEntries(t, cfg))
			}
			assertGortexPreToolUseHooks(t, cfg)
		})
	}
}
