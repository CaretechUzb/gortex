package hooks

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zzet/gortex/internal/daemon"
)

// The daemon-socket fallback must only fire when a hook CWD has been recorded,
// so the pure-HTTP unit tests (which never set one) keep their "no bridge"
// semantics and never touch a real daemon.
func TestCallServerToolDaemonFallbackGatedOnHookCWD(t *testing.T) {
	var called int
	old := callServerToolDaemonFn
	callServerToolDaemonFn = func(_, name string, _ map[string]any) string {
		called++
		return "daemon:" + name
	}
	t.Cleanup(func() { callServerToolDaemonFn = old })

	// No hook CWD → HTTP (port 0, unreachable) fails and the fallback is
	// skipped, leaving the historical empty-string result.
	setHookCWD("")
	if got := callServerTool(0, "detect_changes", nil); got != "" {
		t.Fatalf("want empty result without a hook CWD, got %q", got)
	}
	if called != 0 {
		t.Fatalf("daemon fallback must not fire without a hook CWD, fired %d times", called)
	}

	// With a hook CWD set, an unreachable HTTP surface falls back to the
	// daemon socket scoped to that CWD.
	setHookCWD("/repo")
	defer setHookCWD("")
	if got := callServerTool(0, "detect_changes", nil); got != "daemon:detect_changes" {
		t.Fatalf("want daemon fallback result, got %q", got)
	}
	if called != 1 {
		t.Fatalf("daemon fallback should fire exactly once, fired %d times", called)
	}
}

func TestHookMCPHandshakePinsInternalCompatibilitySurface(t *testing.T) {
	t.Parallel()

	hs := hookMCPHandshake("/repo")
	if hs.Mode != daemon.ModeMCP || hs.ClientName != "gortex-hook" || hs.CWD != "/repo" {
		t.Fatalf("unexpected hook handshake identity: %#v", hs)
	}
	// Deliberately unrestricted: the hooks call tools outside every preset
	// (see the constant's comment). A restricted surface makes those calls
	// return "tool not found", which a hook renders as silence.
	if hookInternalToolSurface != "full" {
		t.Fatalf("internal hook surface = %q, want full", hookInternalToolSurface)
	}
	if hs.Tools != hookInternalToolSurface || hs.ToolsMode != "defer" {
		t.Fatalf("hook handshake surface = %q/%q, want full/defer", hs.Tools, hs.ToolsMode)
	}
}

// corePresetToolsForHookGuard mirrors internal/mcp's corePresetTools. It is
// duplicated rather than imported because internal/hooks must not depend on
// internal/mcp — a hook talks to the daemon over a socket, it does not link
// the server. Drift here can only make the guard below stricter than reality,
// which fails loudly rather than silently.
var corePresetToolsForHookGuard = map[string]bool{
	"explore": true, "smart_context": true, "get_repo_outline": true,
	"graph_stats": true, "index_health": true,
	"search_symbols": true, "search_text": true, "find_files": true,
	"find_usages": true, "find_implementations": true, "get_callers": true,
	"get_call_chain": true, "get_dependencies": true, "get_dependents": true,
	"read_file": true, "get_symbol": true, "get_symbol_source": true,
	"get_file_summary": true, "get_editing_context": true,
	"edit_file": true, "edit_symbol": true, "write_file": true,
	"batch_edit": true, "rename_symbol": true,
	"verify_change": true, "get_diagnostics": true, "check_guards": true,
	"get_test_targets": true, "analyze": true,
	"detect_changes": true, "diff_context": true, "review": true,
	"surface_memories": true, "save_note": true, "store_memory": true,
}

var hookToolCallRe = regexp.MustCompile(`callServerTool\w*\([^,]+,\s*"([a-z_]+)"`)

// hookCalledToolNames scans the package source for every tool the hooks
// dispatch by name, so a newly added call site is picked up automatically
// instead of relying on a hand-maintained list.
func hookCalledToolNames(t *testing.T) map[string][]string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	out := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			continue
		}
		for _, m := range hookToolCallRe.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = append(out[m[1]], name)
		}
	}
	if len(out) == 0 {
		t.Fatal("found no callServerTool call sites — the scanner regex has drifted")
	}
	return out
}

// TestHookToolSurfaceCoversEveryCalledTool is the tripwire for a silent-failure
// class that already bit this package.
//
// A hook dispatches by name over the daemon socket and treats every failure —
// including "tool not found" — as an unreachable bridge, rendering nothing. So
// a tool the declared surface does not cover does not error; its whole section
// just disappears. That is how the Stop briefing's contracts section and the
// PreCompact churn section came to be dead in the default posture.
//
// Either the surface imposes no restriction, or every tool called is inside the
// restricted preset. Anything else is a silent-failure bug.
func TestHookToolSurfaceCoversEveryCalledTool(t *testing.T) {
	unrestricted := map[string]bool{"": true, "full": true, "all": true}
	if unrestricted[hookInternalToolSurface] {
		return
	}

	var offenders []string
	for tool, files := range hookCalledToolNames(t) {
		if !corePresetToolsForHookGuard[tool] {
			offenders = append(offenders, tool+" (called from "+strings.Join(files, ", ")+")")
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("hookInternalToolSurface = %q, but these tools fall outside it and will\n"+
			"silently render nothing instead of failing loudly:\n  %s\n\n"+
			"Either widen hookInternalToolSurface or stop calling these tools.",
			hookInternalToolSurface, strings.Join(offenders, "\n  "))
	}
}

// TestHookCallsKnownNonCoreTools records which calls depend on the wide
// surface, so narrowing it later is a deliberate decision with a visible cost.
func TestHookCallsKnownNonCoreTools(t *testing.T) {
	called := hookCalledToolNames(t)
	for _, tool := range []string{"contracts", "explain_change_impact", "get_symbol_history", "feedback"} {
		if _, ok := called[tool]; !ok {
			t.Errorf("%q is no longer called by the hooks package — drop it from this list", tool)
			continue
		}
		if corePresetToolsForHookGuard[tool] {
			t.Errorf("%q is now in the core preset — the wide-surface justification for it is stale", tool)
		}
	}
}
