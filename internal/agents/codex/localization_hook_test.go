package codex

import (
	"regexp"
	"testing"

	"github.com/zzet/gortex/internal/agents"
)

func TestCodexLocalizationLifecycleMatchersCoverEveryFacadeAndNamespace(t *testing.T) {
	env := codexGlobalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := readCodexConfig(t, env)
	requireHookEntry(t, cfg, "PreToolUse", codexPreToolUseMatcher, testCodexHookCommand)
	requireHookEntry(t, cfg, "PostToolUse", codexPostToolUseMatcher, testCodexHookCommand)

	preMatcher := regexp.MustCompile(codexPreToolUseMatcher)
	postMatcher := regexp.MustCompile(codexPostToolUseMatcher)
	for _, prefix := range []string{"mcp__gortex__", "gortex__"} {
		for _, operation := range []string{"explore", "search", "read", "relations", "trace", "analyze"} {
			tool := prefix + operation
			if !preMatcher.MatchString(tool) {
				t.Errorf("PreToolUse matcher does not cover %q", tool)
			}
			if !postMatcher.MatchString(tool) {
				t.Errorf("PostToolUse matcher does not cover %q", tool)
			}
		}
	}

	state := Inspect(env.Home)
	if state.Hooks["PreToolUse"] != 1 || state.Hooks["PostToolUse"] != 1 {
		t.Fatalf("doctor did not recognize localization hooks: %+v", state.Hooks)
	}
}
