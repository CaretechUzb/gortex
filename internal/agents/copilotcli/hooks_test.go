package copilotcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/agents"
	"github.com/zzet/gortex/internal/agents/agentstest"
)

// hookEvents reads the hooks object out of the written config.
func hookEvents(t *testing.T, env agents.Env) map[string][]map[string]any {
	t.Helper()
	cfg := agentstest.ReadJSON(t, HookConfigPath(env))
	raw, ok := cfg["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("missing top-level hooks object: %v", cfg)
	}
	out := make(map[string][]map[string]any, len(raw))
	for event, v := range raw {
		list, ok := v.([]any)
		if !ok {
			t.Fatalf("event %s is not a list: %v", event, v)
		}
		for _, e := range list {
			entry, ok := e.(map[string]any)
			if !ok {
				t.Fatalf("event %s carries a non-object entry: %v", event, e)
			}
			out[event] = append(out[event], entry)
		}
	}
	return out
}

func sortedEventNames(events map[string][]map[string]any) []string {
	names := make([]string, 0, len(events))
	for name := range events {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestHookConfigShape pins every vendor fact the writer encodes: the
// camelCase event names, the four-event budget, the anchored lowercase
// matchers, and the two per-entry command fields.
func TestHookConfigShape(t *testing.T) {
	env, _ := globalEnv(t)
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	cfg := agentstest.ReadJSON(t, HookConfigPath(env))
	if cfg["version"] != float64(hookConfigVersion) {
		t.Fatalf("version = %v, want %d", cfg["version"], hookConfigVersion)
	}
	if cfg["disableAllHooks"] != false {
		t.Fatalf("disableAllHooks = %v, want false", cfg["disableAllHooks"])
	}

	events := hookEvents(t, env)
	want := []string{"postToolUse", "preToolUse", "sessionStart", "userPromptSubmitted"}
	if got := sortedEventNames(events); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("registered events = %v, want exactly %v — each extra event costs a subprocess per occurrence", got, want)
	}

	for event, entries := range events {
		if len(entries) != 1 {
			t.Fatalf("event %s has %d entries, want 1", event, len(entries))
		}
		entry := entries[0]
		if entry["type"] != "command" {
			t.Errorf("%s type = %v, want command", event, entry["type"])
		}
		// Both shells, always: the CLI picks by platform, so a POSIX-only
		// entry leaves Windows users with a hook that never runs.
		for _, key := range []string{"bash", "powershell"} {
			cmd, _ := entry[key].(string)
			if cmd == "" {
				t.Errorf("%s is missing a %s command", event, key)
				continue
			}
			if !strings.Contains(cmd, "--agent="+hookAgentFlag) {
				t.Errorf("%s %s command does not route to the copilot decoder: %q", event, key, cmd)
			}
			if strings.Contains(cmd, filepath.Clean(os.TempDir())+string(os.PathSeparator)+"go-build") {
				t.Errorf("%s %s baked an ephemeral build path: %q", event, key, cmd)
			}
		}
		if entry["cwd"] != "." {
			t.Errorf("%s cwd = %v, want \".\"", event, entry["cwd"])
		}
		if entry["timeoutSec"] != float64(hookTimeoutSeconds) {
			t.Errorf("%s timeoutSec = %v, want %d", event, entry["timeoutSec"], hookTimeoutSeconds)
		}

		matcher, hasMatcher := entry["matcher"].(string)
		switch event {
		case "preToolUse", "postToolUse":
			if !hasMatcher {
				t.Fatalf("%s must carry a tool matcher", event)
			}
			if !strings.HasPrefix(matcher, "^(?:") || !strings.HasSuffix(matcher, ")$") {
				t.Errorf("%s matcher is not anchored: %q", event, matcher)
			}
			if _, err := regexp.Compile(matcher); err != nil {
				t.Errorf("%s matcher does not compile: %v", event, err)
			}
			names := strings.Split(strings.TrimSuffix(strings.TrimPrefix(matcher, "^(?:"), ")$"), "|")
			present := make(map[string]bool, len(names))
			for _, n := range names {
				if n != strings.ToLower(n) {
					t.Errorf("%s matcher lists %q; the CLI's built-in tool names are lowercase", event, n)
				}
				present[n] = true
			}
			// powershell is a first-class sibling of bash. Omitting it
			// leaves the whole Windows shell surface unenforced.
			for _, required := range []string{"view", "grep", "rg", "glob", "bash", "powershell", "create", "edit"} {
				if !present[required] {
					t.Errorf("%s matcher does not cover %q: %q", event, required, matcher)
				}
			}
		default:
			if hasMatcher {
				t.Errorf("%s carries a matcher (%q) but has no tool name to match against", event, matcher)
			}
		}
	}
}

// TestHooksNotWrittenWhenDisabled: --no-hooks must leave the file absent,
// not present-and-empty.
func TestHooksNotWrittenWhenDisabled(t *testing.T) {
	env, _ := globalEnv(t)
	env.InstallHooks = false
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if _, err := os.Stat(HookConfigPath(env)); !os.IsNotExist(err) {
		t.Fatalf("hook config written despite InstallHooks=false (stat err = %v)", err)
	}
}

// TestNeverWritesRepoHooks: .github/hooks/*.json is committed, so it would
// fire gortex on every teammate's clone — including the ones with no
// gortex installed.
func TestNeverWritesRepoHooks(t *testing.T) {
	for _, mode := range []agents.Mode{agents.ModeProject, agents.ModeGlobal} {
		env, _ := newCopilotEnv(t)
		env.Mode = mode
		if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
		if _, err := os.Stat(filepath.Join(env.Root, ".github", "hooks")); !os.IsNotExist(err) {
			t.Errorf("mode %v: .github/hooks must not be written (stat err = %v)", mode, err)
		}
	}
}

// TestHookUpsertNeverDuplicates: N installs leave one entry per event.
func TestHookUpsertNeverDuplicates(t *testing.T) {
	env, _ := globalEnv(t)
	a := New()
	for range 3 {
		if _, err := a.Apply(env, agents.ApplyOpts{}); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	for event, entries := range hookEvents(t, env) {
		if len(entries) != 1 {
			t.Fatalf("event %s accumulated %d entries across re-runs", event, len(entries))
		}
	}
}

// seedHooks writes a hook config verbatim so a test can plant user state.
func seedHooks(t *testing.T, env agents.Env, root map[string]any) {
	t.Helper()
	agentstest.WriteJSON(t, HookConfigPath(env), root)
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestUserHookEntriesSurvive: entries we did not author are the user's,
// on our events and on events we never register.
func TestUserHookEntriesSurvive(t *testing.T) {
	env, _ := globalEnv(t)
	userSessionEnd := map[string]any{
		"type": "command", "bash": "echo bye", "powershell": "Write-Output bye",
	}
	userPreTool := map[string]any{
		"type": "command", "matcher": "^(?:web_fetch)$",
		"bash": "echo fetch", "powershell": "Write-Output fetch",
	}
	seedHooks(t, env, map[string]any{
		"version":         1,
		"disableAllHooks": false,
		"hooks": map[string]any{
			"sessionEnd": []any{userSessionEnd},
			"preToolUse": []any{userPreTool},
		},
		// An unrelated top-level key a future CLI version might add.
		"description": "team hooks",
	})

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	cfg := agentstest.ReadJSON(t, HookConfigPath(env))
	if cfg["description"] != "team hooks" {
		t.Errorf("unrelated top-level key was dropped: %v", cfg)
	}
	events := hookEvents(t, env)
	if got := events["sessionEnd"]; len(got) != 1 || mustJSON(t, got[0]) != mustJSON(t, userSessionEnd) {
		t.Errorf("user entry on an unregistered event changed:\n%v", got)
	}
	pre := events["preToolUse"]
	if len(pre) != 2 {
		t.Fatalf("preToolUse = %d entries, want the user's + ours", len(pre))
	}
	if mustJSON(t, pre[0]) != mustJSON(t, userPreTool) {
		t.Errorf("user preToolUse entry changed:\n%s\n---\n%s", mustJSON(t, userPreTool), mustJSON(t, pre[0]))
	}
	if !isGortexCopilotHookEntry(pre[1]) {
		t.Errorf("our preToolUse entry was not appended: %v", pre[1])
	}
}

// TestUserNarrowedMatcherIsPreserved: a user who trimmed our alternation
// deliberately scoped the hook. Silently re-widening it on the next
// install is how hooks get turned off wholesale — so only --force does.
func TestUserNarrowedMatcherIsPreserved(t *testing.T) {
	env, _ := globalEnv(t)
	narrowed := map[string]any{
		"type":       "command",
		"matcher":    "^(?:bash|powershell)$",
		"bash":       bashHookCommand(env),
		"powershell": powershellHookCommand(env),
		"cwd":        ".",
		"timeoutSec": hookTimeoutSeconds,
	}
	seed := func() {
		seedHooks(t, env, map[string]any{
			"version":         1,
			"disableAllHooks": false,
			"hooks":           map[string]any{"preToolUse": []any{narrowed}},
		})
	}

	seed()
	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	pre := hookEvents(t, env)["preToolUse"]
	if len(pre) != 1 {
		t.Fatalf("preToolUse = %d entries, want the narrowed one alone", len(pre))
	}
	if pre[0]["matcher"] != "^(?:bash|powershell)$" {
		t.Fatalf("narrowed matcher was re-widened without --force: %v", pre[0]["matcher"])
	}

	seed()
	if _, err := New().Apply(env, agents.ApplyOpts{Force: true}); err != nil {
		t.Fatalf("forced apply: %v", err)
	}
	pre = hookEvents(t, env)["preToolUse"]
	if len(pre) != 1 {
		t.Fatalf("forced preToolUse = %d entries, want 1", len(pre))
	}
	if pre[0]["matcher"] != copilotToolMatcher() {
		t.Fatalf("--force did not restore our matcher: %v", pre[0]["matcher"])
	}
}

// TestStaleGortexHookEntryIsReplaced: an entry from an older gortex — a
// dead binary path, a retired flag — is upgraded in place rather than
// left beside a fresh duplicate.
func TestStaleGortexHookEntryIsReplaced(t *testing.T) {
	env, _ := globalEnv(t)
	seedHooks(t, env, map[string]any{
		"version":         1,
		"disableAllHooks": false,
		"hooks": map[string]any{
			"sessionStart": []any{map[string]any{
				"type":       "command",
				"bash":       "/opt/old/gortex hook --agent=copilot-cli --mode=deny",
				"powershell": "& '/opt/old/gortex' hook --agent=copilot-cli --mode=deny",
				"timeoutSec": 99,
			}},
		},
	})

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	start := hookEvents(t, env)["sessionStart"]
	if len(start) != 1 {
		t.Fatalf("sessionStart = %d entries, want the stale one replaced not duplicated", len(start))
	}
	if got, _ := start[0]["bash"].(string); got != bashHookCommand(env) {
		t.Fatalf("stale command survived: %q", got)
	}
}

// TestForeignHookShapeIsLeftAlone: an event whose value is not a list is
// a shape we do not understand and must not reshape.
func TestForeignHookShapeIsLeftAlone(t *testing.T) {
	env, _ := globalEnv(t)
	seedHooks(t, env, map[string]any{
		"version":         1,
		"disableAllHooks": false,
		"hooks":           map[string]any{"preToolUse": "not-a-list"},
	})

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := agentstest.ReadJSON(t, HookConfigPath(env))
	hooks, _ := cfg["hooks"].(map[string]any)
	if hooks["preToolUse"] != "not-a-list" {
		t.Fatalf("foreign preToolUse value was rewritten: %v", hooks["preToolUse"])
	}
	// The events we do understand are still installed.
	if _, ok := hooks["sessionStart"]; !ok {
		t.Error("the remaining events must still be written")
	}
}

// TestDisableAllHooksIsNotFlippedBack: it is a user kill-switch, not a
// field to correct.
func TestDisableAllHooksIsNotFlippedBack(t *testing.T) {
	env, _ := globalEnv(t)
	seedHooks(t, env, map[string]any{"version": 1, "disableAllHooks": true})

	if _, err := New().Apply(env, agents.ApplyOpts{}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	cfg := agentstest.ReadJSON(t, HookConfigPath(env))
	if cfg["disableAllHooks"] != true {
		t.Fatalf("disableAllHooks = %v, want the user's true preserved", cfg["disableAllHooks"])
	}
}

// TestHookBinaryDropsTheSubcommand: Env.HookCommand is "<binary> hook";
// the PowerShell field has to quote the path on its own, which is
// impossible once binary and arguments are one string.
func TestHookBinaryDropsTheSubcommand(t *testing.T) {
	// The ordinary case: no quoting, and so no "&" for encoding/json to
	// escape into an unreadable & in the written file.
	plain := agents.Env{HookCommand: "/usr/local/bin/gortex hook"}
	if got := bashHookCommand(plain); got != "/usr/local/bin/gortex hook --agent=copilot-cli" {
		t.Fatalf("bash command = %q", got)
	}
	if got := powershellHookCommand(plain); got != "/usr/local/bin/gortex hook --agent=copilot-cli" {
		t.Fatalf("powershell command = %q; a bare path needs no call operator", got)
	}

	env := agents.Env{HookCommand: "/opt/gortex bin/gortex hook"}
	if got := hookBinary(env); got != "/opt/gortex bin/gortex" {
		t.Fatalf("hookBinary = %q, want the binary without the subcommand", got)
	}
	if got := bashHookCommand(env); got != `'/opt/gortex bin/gortex' hook --agent=copilot-cli` {
		t.Fatalf("bash command = %q; a path with a space must be quoted", got)
	}
	if got := powershellHookCommand(env); got != `& '/opt/gortex bin/gortex' hook --agent=copilot-cli` {
		t.Fatalf("powershell command = %q; needs the & call operator and quoting", got)
	}
}

// TestWindowsHookBinaryIsUsable: a native Windows path must survive into
// both fields — forward-slashed for Git Bash, verbatim for PowerShell.
func TestWindowsHookBinaryIsUsable(t *testing.T) {
	env := agents.Env{HookCommand: `C:\Program Files\gortex\gortex.exe hook`}
	if got := bashHookCommand(env); got != `'C:/Program Files/gortex/gortex.exe' hook --agent=copilot-cli` {
		t.Fatalf("bash command = %q", got)
	}
	if got := powershellHookCommand(env); got != `& 'C:\Program Files\gortex\gortex.exe' hook --agent=copilot-cli` {
		t.Fatalf("powershell command = %q", got)
	}
}

func TestMatcherNarrowingDetection(t *testing.T) {
	want := map[string]any{"matcher": copilotToolMatcher()}
	cases := []struct {
		matcher  string
		narrowed bool
	}{
		{"^(?:bash)$", true},
		{"^(?:bash|powershell)$", true},
		{copilotToolMatcher(), false},   // identical, not narrowed
		{"^(?:bash|web_fetch)$", false}, // widened with a tool we never listed
		{"bash", false},                 // unanchored: not a shape we read
		{"^(?:bash|create|edit|glob|grep|powershell|rg|view|task)$", false},
	}
	for _, tc := range cases {
		got := copilotHookMatcherIsNarrowed(map[string]any{"matcher": tc.matcher}, want)
		if got != tc.narrowed {
			t.Errorf("narrowed(%q) = %v, want %v", tc.matcher, got, tc.narrowed)
		}
	}
}
