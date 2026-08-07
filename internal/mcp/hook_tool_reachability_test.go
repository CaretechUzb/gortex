package mcp

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A hook dispatches a tool by name over the daemon socket and treats every
// failure — "tool not found" included — as an unreachable bridge, rendering
// nothing. A name the server no longer registers therefore produces silence,
// not an error: it survives review, it survives CI, and it is found by accident
// much later. That is how the Stop briefing's contracts section and the
// PreCompact churn section went dead (#356), and how the PreCompact bridge kept
// posting to an HTTP surface that had been removed (#241).
//
// internal/hooks owns a companion guard (TestHookToolSurfaceCoversEveryCalledTool)
// which asks whether a called tool sits inside the declared preset. That guard
// short-circuits whenever the surface is unrestricted, which it is today
// ("full"), and an unrestricted surface is not the same claim as "this name is
// still registered" — a retired tool passes it either way.
//
// This is the missing half: does the name resolve against the registry at all.
// It lives here rather than in internal/hooks because that package deliberately
// does not link the server (a hook talks over a socket; keeping it free of
// internal/mcp is what allows a slim hook client). Reading the hook sources as
// text introduces no such dependency in either direction.

// hookToolCallSite matches the two shapes a hook uses to name a tool: the
// shared dispatcher — callServerTool(port, "name", args) — and the raw
// tools/call frame's "name" member.
var hookToolCallSite = regexp.MustCompile(
	`callServerTool[A-Za-z]*\([^,]+,\s*"([a-z][a-z0-9_]*)"|"name":\s*"([a-z][a-z0-9_]*)"`,
)

// hookDispatchedToolNames reads the non-test sources of internal/hooks and
// returns every tool name a hook asks the daemon to run, mapped to the files
// that call it. Derived from source rather than a hand-kept list so a new call
// site is covered the moment it is written.
func hookDispatchedToolNames(t *testing.T) map[string][]string {
	t.Helper()
	dir := filepath.Join("..", "hooks")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err, "read internal/hooks")

	out := map[string][]string{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, readErr := os.ReadFile(filepath.Clean(filepath.Join(dir, name)))
		require.NoError(t, readErr)
		for _, m := range hookToolCallSite.FindAllStringSubmatch(string(src), -1) {
			tool := m[1]
			if tool == "" {
				tool = m[2]
			}
			if tool != "" {
				out[tool] = append(out[tool], name)
			}
		}
	}
	return out
}

// Every tool a hook dispatches must still resolve against this package's
// registry — as a facade tool in its own right, or as an implementation-era
// name the facade registry still maps to a public operation. A name that is
// neither is one the daemon will not answer, and the hook calling it goes
// quiet instead of failing.
func TestHookDispatchedToolsResolve(t *testing.T) {
	called := hookDispatchedToolNames(t)
	require.NotEmpty(t, called,
		"no tool names found in internal/hooks — the call-site pattern drifted and this guard is now vacuous")

	var unreachable []string
	for tool, files := range called {
		if isFacadeToolName(tool) {
			continue
		}
		if _, _, ok := PublicOperationForLegacy(tool); ok {
			continue
		}
		unreachable = append(unreachable, tool+" (called from "+strings.Join(files, ", ")+")")
	}
	sort.Strings(unreachable)

	assert.Empty(t, unreachable,
		"these hook-dispatched tools resolve to nothing this package registers, so the hooks calling them\n"+
			"render silence rather than an error:\n  %s", strings.Join(unreachable, "\n  "))
}

// The scan is this guard's foundation: if the pattern stops matching real call
// sites, the test above passes with an empty set and the protection disappears
// without a signal. Pin names that are dispatched today so a change to the
// dispatcher's shape fails here instead of quietly blinding the guard.
func TestHookToolScanStillSeesKnownCallSites(t *testing.T) {
	called := hookDispatchedToolNames(t)
	for _, expect := range []string{"detect_changes", "get_file_summary", "contracts"} {
		assert.Contains(t, called, expect,
			"the call-site scan no longer sees %q — the guard has gone blind", expect)
	}
}
