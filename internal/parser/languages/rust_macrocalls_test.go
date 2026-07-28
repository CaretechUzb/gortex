package languages

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// rustCallTargets collects every EdgeCalls target emitted for src.
func rustCallTargets(t *testing.T, src string) map[string]*graph.Edge {
	t.Helper()
	e := NewRustExtractor()
	result, err := e.Extract("probe.rs", []byte(src))
	require.NoError(t, err)
	out := map[string]*graph.Edge{}
	for _, ed := range result.Edges {
		if ed.Kind == graph.EdgeCalls {
			out[ed.To] = ed
		}
	}
	return out
}

// Rust does not parse macro arguments as expressions, so a call written
// inside one is invisible to a query over call_expression. Test suites
// are hit hardest: the call a #[test] actually exercises is normally the
// one inside assert_eq!.
func TestRsExtractor_CallsInsideMacroBodies(t *testing.T) {
	calls := rustCallTargets(t, `fn subject() -> u32 { 1 }
#[test]
fn it_works() {
    assert_eq!(subject(), 1);
    println!("{}", helper());
}
`)
	require.Contains(t, calls, "unresolved::subject",
		"the call a #[test] exercises must be recorded even inside assert_eq!")
	assert.Equal(t, "assert_eq", calls["unresolved::subject"].Meta["via_macro"],
		"a token-recovered site is labelled with the macro it came from")
	require.Contains(t, calls, "unresolved::helper")
}

func TestRsExtractor_MacroBodyCallShapes(t *testing.T) {
	calls := rustCallTargets(t, `fn f() {
    let v = vec![Foo::new()];
    write!(out, "{}", self.name());
}
`)
	require.Contains(t, calls, "unresolved::new", "a path call inside a macro body")
	assert.Equal(t, "Foo::new", calls["unresolved::new"].Meta["rust_path"])
	require.Contains(t, calls, "unresolved::*.name", "a method call inside a macro body")
	assert.Equal(t, "self", calls["unresolved::*.name"].Meta["rust_recv"])
}

// The invocation itself is a use of the macro, so a macro_rules!
// definition gets incoming edges.
func TestRsExtractor_MacroInvocationRecordsUse(t *testing.T) {
	calls := rustCallTargets(t, `macro_rules! log_it { () => {} }
fn f() { log_it!(); }
`)
	require.Contains(t, calls, "unresolved::log_it")
	assert.Equal(t, true, calls["unresolved::log_it"].Meta["rust_macro"])
}

// Pattern and cfg-predicate macros hold patterns, not expressions.
// Scanning them would mint calls to things that are not functions.
func TestRsExtractor_PatternMacrosAreNotScanned(t *testing.T) {
	calls := rustCallTargets(t, `fn f(x: Option<u8>) {
    let _ = matches!(x, Some(_));
    let _ = cfg!(any(unix, windows));
}
`)
	assert.NotContains(t, calls, "unresolved::Some",
		"matches! holds a pattern — Some(_) is not a call")
	assert.NotContains(t, calls, "unresolved::any",
		"cfg! holds a predicate — any(..) is not a call")
}

// Prelude constructors dominate macro bodies but carry no call-graph
// value: assert_eq!(f(), Ok(3)) is a statement about f, not about Ok.
func TestRsExtractor_PreludeConstructorsSkippedInMacros(t *testing.T) {
	calls := rustCallTargets(t, `fn f() { assert_eq!(compute(), Ok(3)); }`)
	require.Contains(t, calls, "unresolved::compute")
	assert.NotContains(t, calls, "unresolved::Ok")
}

// A trailing ::<T> wraps the callee in a generic_function node, which
// none of the three original call patterns matched.
func TestRsExtractor_TurbofishCallSites(t *testing.T) {
	calls := rustCallTargets(t, `fn f(items: Vec<u32>) {
    let a = items.iter().collect::<Vec<u32>>();
    let b = parse::<u32>("3");
}
`)
	require.Contains(t, calls, "unresolved::*.collect",
		"xs.collect::<Vec<_>>() is a method call")
	require.Contains(t, calls, "unresolved::parse",
		"parse::<u32>(s) is a plain call")
}
