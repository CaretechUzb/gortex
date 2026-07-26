package hooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestRunPostTask_RejectsWrongEvent(t *testing.T) {
	data := []byte(`{"hook_event_name":"PreToolUse"}`)
	out := captureStdout(t, func() { runPostTask(data, 0) })
	if out != "" {
		t.Errorf("expected silent no-op for wrong event, got: %q", out)
	}
}

func TestRunPostTask_StopHookActive_Skips(t *testing.T) {
	// stop_hook_active=true means we're already inside a Stop-hook loop;
	// firing again would recurse.
	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":true}`)
	out := captureStdout(t, func() { runPostTask(data, 1) })
	if out != "" {
		t.Errorf("expected no output when stop_hook_active, got: %q", out)
	}
}

func TestRunPostTask_NoBridge(t *testing.T) {
	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, 1) })
	if out != "" {
		t.Errorf("expected no output when bridge unreachable, got: %q", out)
	}
}

func TestRunPostTask_NoChanges_Silent(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":[],"changed_symbols":[],"risk":"NONE","summary":"no indexed symbols affected"}`,
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })
	if out != "" {
		t.Errorf("expected silent no-op when nothing changed, got: %q", out)
	}
}

func TestRunPostTask_RendersDiagnostics(t *testing.T) {
	changedJSON := `{
		"changed_files":["internal/foo.go","internal/bar.go"],
		"changed_symbols":[
			{"id":"internal/foo.go::Foo","name":"Foo","kind":"function"},
			{"id":"internal/bar.go::Bar","name":"Bar","kind":"method"}
		],
		"risk":"MEDIUM",
		"summary":"2 symbols touched"
	}`
	srv := newFakeServer(map[string]string{
		"detect_changes":   changedJSON,
		"get_test_targets": "internal/foo_test.go::TestFoo\ninternal/bar_test.go::TestBar",
		"check_guards":     "boundary my-rule cross-layer import violates ui→db\n",
		"analyze":          "function Orphan internal/foo.go::Foo unused fan_in=0\n",
		"contracts":        "matched: 0 pairs\norphan providers: 1\n  [gortex] http::GET::/api/ghost api.go:12\norphan consumers: 0\n",
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, port) })

	if out == "" {
		t.Fatal("expected diagnostic output when changes are present")
	}
	var payload HookOutput
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output not valid HookOutput JSON: %v\n%s", err, out)
	}
	if payload.HookSpecificOutput == nil || payload.HookSpecificOutput.HookEventName != "Stop" {
		t.Fatal("hookSpecificOutput missing or wrong event name")
	}

	ac := payload.HookSpecificOutput.AdditionalContext
	mustContain := []string{
		"Working-Tree Diagnostics",
		"risk `MEDIUM`",
		"Tests Covering These Symbols",
		"internal/foo_test.go::TestFoo",
		"Guard Violations",
		"boundary my-rule",
		"Potential Dead Code",
		"internal/foo.go::Foo", // only the changed-symbol intersection
		"API Contract Issues",
		"orphan provider",
	}
	for _, frag := range mustContain {
		if !strings.Contains(ac, frag) {
			t.Errorf("briefing missing %q\n---\n%s", frag, ac)
		}
	}
}

// TestPostTaskBriefing_DisclosesWholeTreeScope pins the honesty contract: the
// briefing must name the tree it diffed, admit it does not attribute changes to
// a session, admit untracked files are invisible, and must not tell the agent to
// run tests for work that may not be its own.
func TestPostTaskBriefing_DisclosesWholeTreeScope(t *testing.T) {
	srv := newRecordingFakeServer(map[string]string{
		"detect_changes": `{
			"changed_files":["internal/foo.go"],
			"changed_symbols":[{"id":"internal/foo.go::Foo","name":"Foo","kind":"function","file_path":"gortex/internal/foo.go"}],
			"risk":"LOW","summary":"1 symbol",
			"scope":"all","repo":"gortex","repo_root":"/tmp/checkout-a"
		}`,
		"get_test_targets": "internal/foo_test.go TestFoo\n",
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })
	if out == "" {
		t.Fatal("expected a briefing")
	}

	for _, frag := range []string{
		"/tmp/checkout-a",    // names the tree actually diffed
		"does not attribute", // admits the limitation
		"whole working tree", // states the real scope
		"Untracked",          // discloses the blind spot
		"parallel session",   // names the concurrent-session case
		"uncommitted (staged + unstaged)",
	} {
		if !strings.Contains(out, frag) {
			t.Errorf("briefing missing %q\n---\n%s", frag, out)
		}
	}
	for _, banned := range []string{
		"Run the tests above", // the old imperative
		"**Changed:**",        // the old session-implying claim
		"Post-Task",           // the old title
	} {
		if strings.Contains(out, banned) {
			t.Errorf("briefing still contains %q\n---\n%s", banned, out)
		}
	}

	// The scope we request must match the scope we claim in prose.
	calls := srv.argsFor("detect_changes")
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 detect_changes call, got %d", len(calls))
	}
	if got := calls[0]["scope"]; got != postTaskDiffScope {
		t.Errorf("detect_changes scope = %v, want %q", got, postTaskDiffScope)
	}
	if got := calls[0]["summary_only"]; got != true {
		t.Errorf("detect_changes summary_only = %v, want true", got)
	}
}

func TestRunPostTask_DeadCodeFiltersToChanged(t *testing.T) {
	// Dead-code results that don't overlap with changed symbols should
	// NOT be included — we only flag what the current task left orphaned.
	changedJSON := `{
		"changed_files":["foo.go"],
		"changed_symbols":[{"id":"foo.go::Foo","name":"Foo","kind":"function"}],
		"risk":"LOW","summary":"1 symbol"
	}`
	srv := newFakeServer(map[string]string{
		"detect_changes":   changedJSON,
		"get_test_targets": "",
		"check_guards":     "",
		"analyze":          "function SomethingElse unrelated/path.go fan_in=0\n",
		// Realistic clean output: the compact shape always leads with the
		// matched pairs and reports zero orphans at the end.
		"contracts": "matched: 2 pairs\n  c1: [a] a.go:1 -> [b] b.go:2\n  c2: [a] a.go:9 -> [b] b.go:4\norphan providers: 0\norphan consumers: 0\n",
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if strings.Contains(out, "Potential Dead Code") {
		t.Errorf("dead-code section should be omitted when no overlap with changed symbols:\n%s", out)
	}
	if strings.Contains(out, "API Contract Issues") {
		t.Errorf("contract section should be omitted when clean:\n%s", out)
	}
}

// contractsCompact builds a realistic `contracts check --compact` payload:
// matched pairs first, orphan counts and rows last.
func contractsCompact(matchedPairs, orphanProviders int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "matched: %d pairs\n", matchedPairs)
	for i := 0; i < matchedPairs; i++ {
		fmt.Fprintf(&b, "  c%d: [a] a.go:%d -> [b] b.go:%d\n", i, i, i)
	}
	fmt.Fprintf(&b, "orphan providers: %d\n", orphanProviders)
	for i := 0; i < orphanProviders; i++ {
		fmt.Fprintf(&b, "  [repo] http::GET::/ghost%d api.go:%d\n", i, i)
	}
	b.WriteString("orphan consumers: 0\n")
	return b.String()
}

// TestPostTaskBriefing_ContractOrphansSurviveManyMatchedPairs is the
// regression test for the truncation bug: the orphan block sits at the END of
// the compact payload, so a leading line cap dropped it entirely once enough
// contracts matched.
func TestPostTaskBriefing_ContractOrphansSurviveManyMatchedPairs(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
		"contracts":      contractsCompact(60, 1),
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if !strings.Contains(out, "API Contract Issues") {
		t.Fatalf("expected the contract section when an orphan exists:\n%s", out)
	}
	if !strings.Contains(out, "/ghost0") {
		t.Errorf("orphan row was truncated away by the matched-pair rows:\n%s", out)
	}
	if strings.Contains(out, "c30:") {
		t.Errorf("matched pairs should not be rendered at all:\n%s", out)
	}
}

// TestPostTaskBriefing_ContractSectionOmittedWhenNoOrphans pins that a clean
// contracts check renders no heading, even though its payload is non-empty.
func TestPostTaskBriefing_ContractSectionOmittedWhenNoOrphans(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
		"contracts":      contractsCompact(60, 0),
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if strings.Contains(out, "API Contract Issues") {
		t.Errorf("contract section should be omitted when both orphan counts are 0:\n%s", out)
	}
}

// TestRenderGuardViolations_SuppressesNoRulesConfigured covers the other clean
// sentinel: guards are configured nowhere, so there is nothing to report.
func TestRenderGuardViolations_SuppressesNoRulesConfigured(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
		"check_guards":   "no guard rules configured\n",
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if strings.Contains(out, "Guard Violations") {
		t.Errorf("guard section should be omitted when no rules are configured:\n%s", out)
	}
}

// TestRenderTestTargets_CapsBytes guards the briefing budget against a tool
// that ignores `compact` and returns one enormous newline-free payload — the
// exact shape that leaked a raw JSON blob into the briefing.
func TestRenderTestTargets_CapsBytes(t *testing.T) {
	blob := `{"test_targets":[` + strings.Repeat(`{"file":"x_test.go","functions":["TestX"]},`, 2000) + `]}`
	if strings.Contains(blob, "\n") {
		t.Fatal("fixture must be a single line to exercise the byte cap")
	}
	srv := newFakeServer(map[string]string{
		"detect_changes":   `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
		"get_test_targets": blob,
	})
	defer srv.Close()

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runPostTask(data, portFromURL(t, srv.URL)) })

	if len(out) > 6000 {
		t.Errorf("briefing grew to %d bytes; the byte cap did not bind", len(out))
	}
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected a truncation marker in the capped section:\n%s", out)
	}
}

func TestDispatch_RoutesStop(t *testing.T) {
	srv := newFakeServer(map[string]string{
		"detect_changes": `{"changed_files":["a.go"],"changed_symbols":[{"id":"a.go::A","name":"A","kind":"function"}],"risk":"LOW","summary":"1"}`,
	})
	defer srv.Close()
	port := portFromURL(t, srv.URL)

	data := []byte(`{"hook_event_name":"Stop","stop_hook_active":false}`)
	out := captureStdout(t, func() { runFromBytes(t, data, port) })
	if !strings.Contains(out, "Working-Tree Diagnostics") {
		t.Errorf("Run did not route to PostTask handler:\n%s", out)
	}
}

// runFromBytes feeds raw bytes into Run() by temporarily swapping stdin.
func runFromBytes(t *testing.T, data []byte, port int) {
	t.Helper()
	withStdin(t, data, func() { Run(port, ModeDeny) })
}
