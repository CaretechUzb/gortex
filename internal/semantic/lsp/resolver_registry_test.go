package lsp

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestResolverHelperRegistry_RegisterAndDispatch(t *testing.T) {
	reg := NewResolverHelperRegistry()

	// Single-repo helper under empty prefix.
	hs := newScriptedHelper(t, "/tmp/single", map[string]scriptedAnswer{
		"src/foo.ts:5:doIt": {defPath: "src/bar.ts", defLine: 10},
	})
	reg.Register("", hs)

	require.True(t, reg.SupportsPath("src/foo.ts"))
	defPath, defLine, ok := reg.Definition("src/foo.ts", 5, "doIt")
	require.True(t, ok)
	assert.Equal(t, "src/bar.ts", defPath, "single-repo mode returns unprefixed path")
	assert.Equal(t, 10, defLine)
}

func TestResolverHelperRegistry_MultiRepoDispatch(t *testing.T) {
	reg := NewResolverHelperRegistry()

	hRepoA := newScriptedHelper(t, "/tmp/A", map[string]scriptedAnswer{
		"a.ts:1:f": {defPath: "lib/a.ts", defLine: 9},
	})
	hRepoB := newScriptedHelper(t, "/tmp/B", map[string]scriptedAnswer{
		"foo/bar.ts:2:g": {defPath: "lib/b.ts", defLine: 4},
	})
	reg.Register("repoA", hRepoA)
	reg.Register("repoB/inner", hRepoB)

	// repoA dispatch
	defPath, _, ok := reg.Definition("repoA/a.ts", 1, "f")
	require.True(t, ok)
	assert.Equal(t, "repoA/lib/a.ts", defPath, "answer re-prefixed with repoA")

	// repoB/inner dispatch (longest-prefix wins over a hypothetical
	// shorter prefix that doesn't exist here)
	defPath2, _, ok2 := reg.Definition("repoB/inner/foo/bar.ts", 2, "g")
	require.True(t, ok2)
	assert.Equal(t, "repoB/inner/lib/b.ts", defPath2)

	// File outside both prefixes → no helper.
	_, _, ok3 := reg.Definition("repoC/x.ts", 1, "h")
	assert.False(t, ok3)
}

func TestResolverHelperRegistry_LongestPrefixWins(t *testing.T) {
	reg := NewResolverHelperRegistry()
	outer := newScriptedHelper(t, "/tmp/outer", map[string]scriptedAnswer{
		"x/y.ts:1:f": {defPath: "outer-defined.ts", defLine: 5},
	})
	inner := newScriptedHelper(t, "/tmp/outer/inner", map[string]scriptedAnswer{
		"y.ts:1:f": {defPath: "inner-defined.ts", defLine: 7},
	})
	reg.Register("outer", outer)
	reg.Register("outer/inner", inner)

	// inner prefix is longer — wins for files inside it.
	defPath, _, ok := reg.Definition("outer/inner/y.ts", 1, "f")
	require.True(t, ok)
	assert.Equal(t, "outer/inner/inner-defined.ts", defPath)

	// outer-only file routes to outer.
	defPath2, _, ok2 := reg.Definition("outer/x/y.ts", 1, "f")
	require.True(t, ok2)
	assert.Equal(t, "outer/outer-defined.ts", defPath2)
}

func TestResolverHelperRegistry_Unregister(t *testing.T) {
	reg := NewResolverHelperRegistry()
	h := newScriptedHelper(t, "/tmp/r", map[string]scriptedAnswer{
		"a.ts:1:f": {defPath: "b.ts", defLine: 4},
	})
	reg.Register("repo", h)

	_, _, ok := reg.Definition("repo/a.ts", 1, "f")
	assert.True(t, ok)

	reg.Unregister("repo")
	_, _, ok = reg.Definition("repo/a.ts", 1, "f")
	assert.False(t, ok)
	assert.False(t, reg.SupportsPath("repo/a.ts"))
}

func TestResolverHelperRegistry_SupportsPath_RoutesByExtension(t *testing.T) {
	reg := NewResolverHelperRegistry()
	h := newScriptedHelper(t, "/tmp/r", nil)
	reg.Register("", h)

	assert.True(t, reg.SupportsPath("src/foo.ts"))
	assert.True(t, reg.SupportsPath("src/foo.tsx"))
	assert.True(t, reg.SupportsPath("src/foo.js"))
	assert.True(t, reg.SupportsPath("src/foo.jsx"))
	assert.False(t, reg.SupportsPath("src/foo.go"))
	assert.False(t, reg.SupportsPath("src/foo.py"))
}

func TestResolverHelperMux_RoutesByExtension(t *testing.T) {
	ts := newScriptedHelperWithExtensions(t, "/tmp/r", []string{".ts"}, map[string]scriptedAnswer{
		"src/foo.ts:3:run": {defPath: "src/foo.ts", defLine: 9},
	})
	py := newScriptedHelperWithExtensions(t, "/tmp/r", []string{".py"}, map[string]scriptedAnswer{
		"src/foo.py:4:run": {defPath: "src/foo.py", defLine: 11},
	})
	mux := NewResolverHelperMux(ts, py)

	assert.True(t, mux.SupportsPath("src/foo.ts"))
	assert.True(t, mux.SupportsPath("src/foo.py"))
	assert.False(t, mux.SupportsPath("src/foo.go"))

	defPath, defLine, ok := mux.Definition("src/foo.py", 4, "run")
	require.True(t, ok)
	assert.Equal(t, "src/foo.py", defPath)
	assert.Equal(t, 11, defLine)

	defPath, defLine, ok = mux.Definition("src/foo.ts", 3, "run")
	require.True(t, ok)
	assert.Equal(t, "src/foo.ts", defPath)
	assert.Equal(t, 9, defLine)
}

// TestNewLazyResolverHelper_LookupRunsPerCall verifies the
// router-backed contract through the public Definition entry point: a
// failing lookup is re-run on the next call rather than poisoning the
// helper. Sticky failures were how a helper went permanently blind
// after one transient router hiccup; the router's own markSpawnFailed
// is what fails a genuinely-broken server fast.
func TestNewLazyResolverHelper_LookupRunsPerCall(t *testing.T) {
	var calls int
	h := NewLazyResolverHelper(
		func() (*Provider, error) {
			calls++
			return nil, errors.New("simulated")
		},
		t.TempDir(),
		nil,
		100*time.Millisecond,
		zap.NewNop(),
	)

	_, _, ok := h.Definition("foo.ts", 1, "x")
	assert.False(t, ok)
	_, _, ok = h.Definition("foo.ts", 2, "y")
	assert.False(t, ok)
	assert.Equal(t, 2, calls, "router-backed lookup must run once per Definition call")
}

func TestResolverHelper_NilSafe(t *testing.T) {
	var h *ResolverHelper
	assert.False(t, h.SupportsPath("foo.ts"))
	_, _, ok := h.Definition("foo.ts", 1, "x")
	assert.False(t, ok)
	assert.NoError(t, h.Close())
}

// --- helpers ---

type scriptedAnswer struct {
	defPath string
	defLine int
}

// scriptedHelper stubs resolver.LSPHelper for registry tests without
// spawning an LSP subprocess. SupportsPath claims the TS-family
// extension set; Definition returns answers from a map keyed by
// "<relPath>:<line>:<name>". Tracks workspace root for diagnostics
// but doesn't open any files.
type scriptedHelper struct {
	workspaceRoot string
	answers       map[string]scriptedAnswer
	extensions    map[string]struct{}
}

func newScriptedHelper(t *testing.T, workspaceRoot string, answers map[string]scriptedAnswer) *scriptedHelper {
	return newScriptedHelperWithExtensions(t, workspaceRoot, []string{
		".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs",
	}, answers)
}

func newScriptedHelperWithExtensions(t *testing.T, workspaceRoot string, extensions []string, answers map[string]scriptedAnswer) *scriptedHelper {
	t.Helper()
	if answers == nil {
		answers = map[string]scriptedAnswer{}
	}
	if abs, err := filepath.Abs(workspaceRoot); err == nil {
		workspaceRoot = abs
	}
	exts := make(map[string]struct{}, len(extensions))
	for _, ext := range extensions {
		exts[strings.ToLower(ext)] = struct{}{}
	}
	return &scriptedHelper{workspaceRoot: workspaceRoot, answers: answers, extensions: exts}
}

func (s *scriptedHelper) SupportsPath(relPath string) bool {
	if s == nil || relPath == "" {
		return false
	}
	ext := strings.ToLower(filepath.Ext(relPath))
	_, ok := s.extensions[ext]
	return ok
}

func (s *scriptedHelper) Definition(relPath string, oneBasedLine int, name string) (string, int, bool) {
	key := fmt.Sprintf("%s:%d:%s", relPath, oneBasedLine, name)
	a, ok := s.answers[key]
	if !ok {
		return "", 0, false
	}
	return a.defPath, a.defLine, true
}

// silence unused-import diagnostics for time/zap/errors when only
// some tests in this file use them.
var (
	_ = time.Millisecond
	_ = zap.NewNop
	_ = errors.New
)
