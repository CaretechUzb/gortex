package mcp

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/elide"
)

func TestParseFidelityGlobs(t *testing.T) {
	rules := parseFidelityGlobs("internal/**:full,*_test.go:omit,vendor/**:compress")
	require.Len(t, rules, 3)
	assert.Equal(t, "internal/**", rules[0].glob)
	assert.Equal(t, elide.FidelityFull, rules[0].fidelity)
	assert.Equal(t, "*_test.go", rules[1].glob)
	assert.Equal(t, elide.FidelityOmit, rules[1].fidelity)
	assert.Equal(t, "vendor/**", rules[2].glob)
	assert.Equal(t, elide.FidelityCompress, rules[2].fidelity)

	// Malformed clauses are skipped, not fatal.
	assert.Empty(t, parseFidelityGlobs(""))
	assert.Empty(t, parseFidelityGlobs("nofidelity"))
	assert.Empty(t, parseFidelityGlobs("glob:bogus"))
	assert.Empty(t, parseFidelityGlobs(":full"))
	mixed := parseFidelityGlobs("good/**:full, ,bad, *.go:omit")
	require.Len(t, mixed, 2, "only the two well-formed clauses survive")
}

func TestMatchFidelityGlob(t *testing.T) {
	cases := []struct {
		pattern string
		rel     string
		want    bool
	}{
		// Trailing /** matches the dir and everything beneath.
		{"internal/**", "internal/foo/bar.go", true},
		{"internal/**", "internal", true},
		{"internal/**", "internalish/x.go", false},
		{"internal/**", "cmd/main.go", false},
		// Basename glob works without a **/ prefix.
		{"*_test.go", "internal/mcp/foo_test.go", true},
		{"*_test.go", "foo_test.go", true},
		{"*_test.go", "foo.go", false},
		// Leading **/ matches any depth.
		{"**/*.go", "a/b/c/x.go", true},
		{"**/testdata/*.json", "a/testdata/fixture.json", true},
		{"**/testdata/*.json", "a/b/testdata/fixture.json", true},
		// Bare ** matches everything.
		{"**", "anything/at/all.rs", true},
		// Bare directory prefix.
		{"vendor", "vendor/x/y.go", true},
		{"vendor", "vendored/x.go", false},
		// Single-segment * never crosses a slash. On Windows this is the
		// whole point of using path.Match: filepath.Match's separator is
		// the platform's, so '/' is an ordinary character there and the
		// second case answers true. Both cases pass on linux/macos with
		// either matcher, so only the Windows runner can tell them apart
		// — see the FidelityGlob selector in ci.yml.
		{"internal/*.go", "internal/x.go", true},
		{"internal/*.go", "internal/sub/x.go", false},
	}
	for _, c := range cases {
		got := matchFidelityGlob(c.pattern, c.rel)
		assert.Equalf(t, c.want, got, "matchFidelityGlob(%q, %q)", c.pattern, c.rel)
	}
}

// TestMatchFidelityGlob_DirStarStaysRecursive pins the one shape where
// the segment rule above does not decide the verdict. A trailing `/*`
// never reaches path.Match for a nested path — matchSegmentGlob's
// directory-prefix shortcut answers first — so `internal/*` has the same
// reach as `internal` and `internal/**`. That predates the path.Match
// change and callers depend on it; this test exists so the compatibility
// is a stated contract rather than an accident, and so a later cleanup of
// the shortcut cannot silently narrow a rule someone already wrote.
func TestMatchFidelityGlob_DirStarStaysRecursive(t *testing.T) {
	const pattern = "internal/*"

	assert.True(t, matchFidelityGlob(pattern, "internal"),
		"the directory itself")
	assert.True(t, matchFidelityGlob(pattern, "internal/a.go"),
		"a direct child")
	assert.True(t, matchFidelityGlob(pattern, "internal/sub/x.go"),
		"a nested child — the documented exception to the segment rule")
	assert.True(t, matchFidelityGlob(pattern, "internal/sub/deep/y.go"),
		"an arbitrarily deep child")

	// The shortcut is still segment-anchored: a sibling that merely
	// starts with the same bytes must not match.
	assert.False(t, matchFidelityGlob(pattern, "internalx/a.go"),
		"a sibling directory sharing the prefix")

	// And the segment-bounded form is still available, unchanged.
	assert.False(t, matchFidelityGlob("internal/*.go", "internal/sub/x.go"),
		"internal/*.go stays segment-bounded")
}

func TestFidelityDecideForPath(t *testing.T) {
	rules := parseFidelityGlobs("internal/**:full,*_test.go:omit")
	// First matching rule wins (order matters).
	dFull := fidelityDecideForPath(rules, "internal/mcp/server.go")
	require.NotNil(t, dFull)
	assert.Equal(t, elide.FidelityFull, dFull(elide.Decl{}))

	dOmit := fidelityDecideForPath(rules, "cmd/foo_test.go")
	require.NotNil(t, dOmit)
	assert.Equal(t, elide.FidelityOmit, dOmit(elide.Decl{}))

	// No matching rule -> nil decider (caller falls back to compress).
	assert.Nil(t, fidelityDecideForPath(rules, "cmd/main.go"))
	assert.Nil(t, fidelityDecideForPath(nil, "anything.go"))
}

// TestReadFile_FidelityGlobsOmit exercises the end-to-end MCP path: a
// fidelity rule that omits every declaration in the matched file
// produces omit markers and drops the bodies, while compress_bodies is
// set so the elide path runs.
func TestReadFile_FidelityGlobsOmit(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:omit",
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, "omitted", "omit rule must leave a marker")
	assert.NotContains(t, content, `strings.Split(t, ".")`,
		"omitted declaration body must be gone")
	assert.NotContains(t, content, "func ValidateToken",
		"omitted declaration signature must be gone")
	assert.Equal(t, true, m["bodies_elided"])
}

// TestReadFile_FidelityGlobsFull asserts a `full` rule leaves the file
// uncompressed (body present, no stub) even with compress_bodies set.
func TestReadFile_FidelityGlobsFull(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:full",
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, `strings.Split(t, ".")`,
		"a full rule must keep the body verbatim")
	assert.NotContains(t, content, "lines elided",
		"a full rule must not stub any body")
}

// TestReadFile_FidelityGlobsCompressFallback asserts that when no rule
// matches the file, the call falls back to the plain compress_bodies
// behaviour (body stubbed, signature kept).
func TestReadFile_FidelityGlobsCompressFallback(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "vendor/**:omit", // does not match service.go
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, "func ValidateToken", "signature kept on compress fallback")
	assert.Contains(t, content, "lines elided", "body stubbed on compress fallback")
	assert.NotContains(t, content, "omitted", "no omit marker when the omit rule does not match")
}

// TestReadFile_FidelityGlobsKeepComposes asserts the per-symbol keep
// predicate overrides an omit rule: the kept symbol survives at full
// source while the rest of the file is omitted.
func TestReadFile_FidelityGlobsKeepComposes(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "read_file", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:omit",
		"keep":            "ValidateToken",
	}))
	content, _ := m["content"].(string)
	require.NotEmpty(t, content)
	assert.Contains(t, content, "func ValidateToken", "kept symbol survives omit rule")
	assert.Contains(t, content, `strings.Split(t, ".")`, "kept symbol keeps its body")
	assert.Contains(t, content, "omitted", "other declarations still omitted")
}

// TestGetEditingContext_FidelityGlobsOmit asserts the same fidelity_globs
// wiring on get_editing_context's source_compressed view.
func TestGetEditingContext_FidelityGlobsOmit(t *testing.T) {
	srv, _ := setupCompressTestServer(t)
	m := extractTextResult(t, callTool(t, srv, "get_editing_context", map[string]any{
		"path":            "service.go",
		"compress_bodies": true,
		"fidelity_globs":  "*.go:omit",
	}))
	sc, _ := m["source_compressed"].(string)
	require.NotEmpty(t, sc, "source_compressed must be present")
	assert.Contains(t, sc, "omitted", "omit rule must mark declarations")
	assert.NotContains(t, sc, `strings.Split(t, ".")`, "omitted body must be gone")
}

// TestMatchFidelityGlob_RepeatedGlobstarsStayBounded is the regression
// test for a denial-of-service, not a performance nicety.
//
// The matcher walks the pattern against the path, and a plain recursion
// re-derives the same (pattern suffix, path suffix) pair once for every
// way of reaching it — so each additional `**` multiplies the work. This
// exact input ran for over a hundred seconds before the memo; `glob` is
// user input and find_files evaluates it against every candidate file
// before applying the result limit, so one request could hold a daemon
// core indefinitely.
//
// The globstars are separated by a literal segment on purpose. Adjacent
// ones collapse in globPatternSegments before the matcher ever sees them,
// so a run of `**/**/**/…` reduces to a single `**` and exercises none of
// the memo — an earlier version of this test used exactly that and stayed
// green with the memo deleted. Alternating `**/x/` survives the collapse
// and is what forces the repeated subproblems.
//
// The size is measured, not guessed. Removing the memo and leaving the
// collapse in place, on this machine:
//
//	globstars  path segments   calls          unmemoised
//	        8             20     803,860           10 ms
//	        8             40 246,777,526          7.48 s
//	       10             30 151,946,378          8.04 s
//	       10             40 3,189,663,472     1 m 53.8 s
//
// The memoised matcher answers every one of those in under a
// millisecond. Ten and forty gives the deadline a margin of more than
// twenty times against the exponential path while sitting several orders
// of magnitude above the memoised one, so a loaded runner cannot trip it
// and a lost memo cannot pass it. Smaller inputs — including 8 and 20 —
// prove the memo matters by call count but finish far inside any
// wall-clock bound, which is how the previous version of this test came
// to be green against a matcher with no memo at all.
func TestMatchFidelityGlob_RepeatedGlobstarsStayBounded(t *testing.T) {
	pattern := "a/" + strings.Repeat("**/x/", 10) + "never"
	rel := "a/" + strings.Repeat("x/", 40) + "q"

	// Guard the guard: if a future change makes these collapse too, the
	// timing assertion below stops testing anything.
	segs := globPatternSegments(pattern)
	globstars := 0
	for _, s := range segs {
		if s == "**" {
			globstars++
		}
	}
	require.Greaterf(t, globstars, 4,
		"the adversarial pattern collapsed to %v — it no longer reaches the memo", segs)

	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- matchFidelityGlob(pattern, rel) }()

	select {
	case got := <-done:
		assert.False(t, got, "the path does not end in `never`, so this must not match")
		assert.Lessf(t, time.Since(start), 5*time.Second,
			"matchFidelityGlob(%q, ...) took %s — the globstar walk is enumerating again",
			pattern, time.Since(start))
	case <-time.After(5 * time.Second):
		t.Fatalf("matchFidelityGlob(%q, ...) did not finish within 5s — "+
			"a user-supplied glob can pin a daemon core", pattern)
	}
}

// TestMatchFidelityGlob_TerminalStarKeepsItsRequiredSegment pins the
// depth an ordinary `*` demands. The trailing-star rewrite exists only to
// let the subtree rule survive a globbed prefix; applied to a pattern
// with no globstar it silently dropped a required segment, because `**`
// may consume zero. A `*/*` find_files glob then returned root files, and
// the same pattern in fidelity_globs applied omit/compress rules to them.
func TestMatchFidelityGlob_TerminalStarKeepsItsRequiredSegment(t *testing.T) {
	for _, tc := range []struct {
		pattern string
		rel     string
		want    bool
	}{
		{"*/*", "top.go", false},
		{"src/*/*", "src/top.go", false},
		// The depth they do accept is unchanged.
		{"*/*", "a/b.go", true},
		{"src/*/*", "src/a/b.go", true},
		// And the rewrite still fires where it is meant to.
		{"src/**/internal/*", "src/a/internal/sub/deep.go", true},
	} {
		assert.Equalf(t, tc.want, matchFidelityGlob(tc.pattern, tc.rel),
			"matchFidelityGlob(%q, %q)", tc.pattern, tc.rel)
	}
}

// TestFidelityGlobTerminalStarDepthAtTheConsumer runs the same depth rule
// through the path a configured fidelity rule actually takes, so a
// widening cannot reach users' files while only the matcher test is
// watched.
func TestFidelityGlobTerminalStarDepthAtTheConsumer(t *testing.T) {
	rules := parseFidelityGlobs("*/*:omit")

	assert.Nil(t, fidelityDecideForPath(rules, "top.go"),
		"a root file must not be caught by `*/*`")

	nested := fidelityDecideForPath(rules, "a/b.go")
	require.NotNil(t, nested, "`*/*` still has to match one level down")
	assert.Equal(t, elide.FidelityOmit, nested(elide.Decl{}))
}

// TestMatchFidelityGlob_GlobstarComposesWithTrailingSubtree pins the two
// documented rules working together: `**` crosses directories anywhere,
// and a trailing `/*` covers a whole subtree. Each held on its own, but
// `src/**/internal/*` resolved neither — the segment walk spent the final
// `*` on one segment, and the legacy prefix fallback cannot help because
// it reads `src/**/internal` literally.
//
// This is also a Windows regression guard: the older filepath.Match
// accepted the deep path here, because '/' is an ordinary character when
// the separator is '\'.
func TestMatchFidelityGlob_GlobstarComposesWithTrailingSubtree(t *testing.T) {
	const pattern = "src/**/internal/*"

	assert.True(t, matchFidelityGlob(pattern, "src/a/internal"),
		"the directory itself, exactly as `internal/*` matches `internal`")
	assert.True(t, matchFidelityGlob(pattern, "src/a/internal/x.go"),
		"a direct child")
	assert.True(t, matchFidelityGlob(pattern, "src/a/internal/sub/deep.go"),
		"a deeply nested child — the case that regressed")
	assert.True(t, matchFidelityGlob(pattern, "src/a/b/c/internal/deep/y.go"),
		"the globstar itself spanning several directories")

	assert.False(t, matchFidelityGlob(pattern, "src/a/other/deep.go"),
		"a sibling directory that is not `internal`")
	assert.False(t, matchFidelityGlob(pattern, "other/a/internal/x.go"),
		"the anchored first segment still has to match")
}

// TestFidelityGlobDecideForPath_SubtreeComposition runs the same
// composition through the consumer that fidelity rules actually reach, so
// the contract is pinned at the level users configure rather than only at
// the matcher.
func TestFidelityGlobDecideForPath_SubtreeComposition(t *testing.T) {
	rules := parseFidelityGlobs("src/**/internal/*:full,**:omit")

	for _, rel := range []string{
		"src/a/internal/x.go",
		"src/a/internal/sub/deep.go",
	} {
		d := fidelityDecideForPath(rules, rel)
		require.NotNilf(t, d, "%s matched no rule at all", rel)
		assert.Equalf(t, elide.FidelityFull, d(elide.Decl{}), "%s should take the first rule", rel)
	}

	other := fidelityDecideForPath(rules, "src/a/other/deep.go")
	require.NotNil(t, other)
	assert.Equal(t, elide.FidelityOmit, other(elide.Decl{}),
		"a path outside the subtree must fall through to the catch-all")
}
