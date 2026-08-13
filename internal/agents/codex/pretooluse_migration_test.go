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

func TestCodexPreToolUseMigrationCollapsesCoLocatedManagedHandlers(t *testing.T) {
	const staleCommand = "/tmp/stale-gortex hook --agent=codex --mode=enrich"
	current := `
[[hooks.PreToolUse.hooks]]
type = "command"
command = "` + testCodexHookCommand + `"
statusMessage = "Current Gortex PreToolUse"
`
	stale := `
[[hooks.PreToolUse.hooks]]
type = "command"
command = "` + staleCommand + `"
statusMessage = "Stale Gortex PreToolUse"
`
	for _, tt := range []struct {
		name     string
		handlers string
		force    bool
	}{
		{name: "duplicate current", handlers: current + current},
		{name: "current then stale", handlers: current + stale},
		{name: "stale then current", handlers: stale + current},
		{name: "force duplicate current", handlers: current + current, force: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			env := codexGlobalEnv(t)
			path := codexConfigPath(env)
			seed := `[[hooks.PreToolUse]]
matcher = ".*"
` + tt.handlers
			if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
				t.Fatalf("seed config: %v", err)
			}

			assertNormalized := func(stage string, cfg map[string]any) {
				t.Helper()
				if count := codexPreToolUseManagedHandlerCount(t, cfg); count != 1 {
					t.Fatalf("%s managed handler invocations=%d want 1: %#v", stage, count, preToolUseEntries(t, cfg))
				}
				if count := hookMatcherCommandCount(t, cfg, "PreToolUse", codexPreToolUseMatcher, testCodexHookCommand); count != 1 {
					t.Fatalf("%s current match-all handler count=%d want 1: %#v", stage, count, preToolUseEntries(t, cfg))
				}
				if hasHookCommand(t, cfg, "PreToolUse", staleCommand) {
					t.Fatalf("%s stale managed handler survived: %#v", stage, preToolUseEntries(t, cfg))
				}
				assertGortexPreToolUseHooks(t, cfg)
			}

			if _, err := InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{Force: tt.force}); err != nil {
				t.Fatalf("collapse co-located managed handlers: %v", err)
			}
			assertNormalized("initial migration", readCodexConfig(t, env))

			if _, err := InstallHooksOnly(env.Stderr, path, env, agents.ApplyOpts{}); err != nil {
				t.Fatalf("reapply hooks: %v", err)
			}
			assertNormalized("idempotent reapply", readCodexConfig(t, env))
		})
	}
}

func codexPreToolUseManagedHandlerCount(t *testing.T, cfg map[string]any) int {
	t.Helper()
	count := 0
	for _, entry := range preToolUseEntries(t, cfg) {
		group, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		handlers, ok := codexHookList(group["hooks"])
		if !ok {
			continue
		}
		for _, handler := range handlers {
			fields, ok := handler.(map[string]any)
			if !ok {
				continue
			}
			command, _ := fields["command"].(string)
			if codexCommandInvokesCodexHook(command) {
				count++
			}
		}
	}
	return count
}
