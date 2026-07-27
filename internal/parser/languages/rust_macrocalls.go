package languages

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Rust's grammar does not parse macro arguments as expressions: every
// `macro_invocation` carries an opaque `token_tree` of raw tokens. A
// call written inside one is therefore invisible to a query over
// `call_expression`, and the amount hiding there is not marginal —
// across 4,000 files of real crate source there are 68,551 macro
// invocations concealing 65,525 call sites, against 238,457 call edges
// the extractor finds everywhere else. Test suites suffer worst: 42,511
// of those invocations are `assert*!`, so the call that a `#[test]`
// actually exercises is precisely the one that goes unrecorded.
//
// scanRustMacroCalls recovers them by walking the token stream for the
// three call shapes Rust can spell, using the anonymous `.` / `::`
// separator tokens to tell them apart.

// rustMacroCall is one call site recovered from a macro body.
type rustMacroCall struct {
	name       string
	receiver   string // receiver identifier for `x.method(...)`
	path       string // full `a::b::c` path for a qualified call
	line       int
	isSelector bool
	macroName  string // the macro whose body it was found in
	isMacro    bool   // the site is itself a nested macro invocation
}

// rustPatternMacros are macros whose token tree holds patterns, cfg
// predicates or literal text rather than expressions. Scanning them
// mints calls to things that are not functions — `matches!(x, Some(_))`
// would otherwise report a call to `Some`, and `cfg!(any(unix, windows))`
// a call to `any`.
var rustPatternMacros = map[string]bool{
	"cfg": true, "cfg_if": true, "cfg_attr": true, "cfg_match": true,
	"matches": true, "assert_matches": true, "debug_assert_matches": true,
	"derive": true, "stringify": true, "concat": true, "concat_idents": true,
	"include": true, "include_str": true, "include_bytes": true,
	"env": true, "option_env": true, "line": true, "file": true,
	"column": true, "module_path": true, "compile_error": true,
}

// rustNonCallIdents are prelude constructors that dominate macro bodies
// but carry no call-graph value: `assert_eq!(f(), Ok(3))` is a statement
// about `f`, not about `Ok`. Across the sample corpus these three alone
// accounted for 5,733 of 17,322 plain-call hits.
var rustNonCallIdents = map[string]bool{
	"Some": true, "Ok": true, "Err": true,
}

// scanRustMacroCalls walks the token trees of a macro_invocation and
// returns every call site it can identify.
func scanRustMacroCalls(inv *sitter.Node, src []byte) []rustMacroCall {
	if inv == nil {
		return nil
	}
	macroName := ""
	if mn := inv.ChildByFieldName("macro"); mn != nil {
		macroName = rustLastPathSegment(mn.Content(src))
	}
	if rustPatternMacros[macroName] {
		return nil
	}
	var out []rustMacroCall
	for i := 0; i < int(inv.ChildCount()); i++ {
		c := inv.Child(i)
		if c != nil && c.Type() == "token_tree" {
			scanRustTokenTree(c, src, macroName, &out)
		}
	}
	return out
}

// scanRustTokenTree is the recursive token walk. It looks for an
// identifier immediately followed by a parenthesised token tree, then
// classifies the site by the token that precedes the identifier.
func scanRustTokenTree(tt *sitter.Node, src []byte, macroName string, out *[]rustMacroCall) {
	n := int(tt.ChildCount())
	for i := 0; i < n; i++ {
		c := tt.Child(i)
		if c == nil {
			continue
		}
		if c.Type() == "token_tree" {
			scanRustTokenTree(c, src, macroName, out)
			continue
		}
		if c.Type() != "identifier" {
			continue
		}
		if i+1 >= n {
			continue
		}
		name := c.Content(src)
		line := int(c.StartPoint().Row) + 1

		// A nested macro invocation: `ident ! ( .. )`. Record the
		// invocation, and only descend into its body when that macro
		// holds expressions.
		if nxt := tt.Child(i + 1); nxt != nil && nxt.Content(src) == "!" {
			if i+2 < n {
				if body := tt.Child(i + 2); body != nil && body.Type() == "token_tree" {
					*out = append(*out, rustMacroCall{
						name: name, line: line, macroName: macroName, isMacro: true,
					})
					if !rustPatternMacros[name] {
						scanRustTokenTree(body, src, name, out)
					}
					i += 2
					continue
				}
			}
			continue
		}

		nxt := tt.Child(i + 1)
		if nxt == nil || nxt.Type() != "token_tree" || !strings.HasPrefix(nxt.Content(src), "(") {
			continue
		}

		// Classify by the separator token in front of the identifier.
		sep, prev := "", (*sitter.Node)(nil)
		if i > 0 {
			prev = tt.Child(i - 1)
			if prev != nil {
				sep = prev.Content(src)
			}
		}
		switch sep {
		case ".":
			recv := ""
			if i >= 2 {
				if r := tt.Child(i - 2); r != nil {
					t := r.Type()
					if t == "identifier" || t == "self" {
						recv = r.Content(src)
					}
				}
			}
			*out = append(*out, rustMacroCall{
				name: name, receiver: recv, line: line,
				isSelector: true, macroName: macroName,
			})
		case "::":
			*out = append(*out, rustMacroCall{
				name: name, line: line, macroName: macroName,
				path: rustMacroCallPath(tt, i, src),
			})
		default:
			if rustNonCallIdents[name] {
				continue
			}
			*out = append(*out, rustMacroCall{
				name: name, line: line, macroName: macroName,
			})
		}
	}
}

// rustMacroCallPath rebuilds the full `a::b::c` path of a qualified call
// by walking backwards over alternating `::` separators and identifiers.
func rustMacroCallPath(tt *sitter.Node, nameIdx int, src []byte) string {
	parts := []string{tt.Child(nameIdx).Content(src)}
	i := nameIdx - 1
	for i >= 1 {
		sep := tt.Child(i)
		if sep == nil || sep.Content(src) != "::" {
			break
		}
		seg := tt.Child(i - 1)
		if seg == nil {
			break
		}
		switch seg.Type() {
		case "identifier", "self", "super", "crate", "metavariable":
			parts = append(parts, seg.Content(src))
		default:
			i = -1
			continue
		}
		i -= 2
	}
	for a, b := 0, len(parts)-1; a < b; a, b = a+1, b-1 {
		parts[a], parts[b] = parts[b], parts[a]
	}
	if len(parts) < 2 {
		return ""
	}
	return strings.Join(parts, "::")
}

// stampRustMacroOrigin records how a call site was recovered. A site
// found by the token scan is a weaker signal than one the grammar
// parsed as a call_expression, so it is labelled rather than silently
// mixed in.
func stampRustMacroOrigin(e *graph.Edge, c rustDeferredCall) {
	if c.viaMacro == "" && !c.isMacro {
		return
	}
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	if c.viaMacro != "" {
		e.Meta["via_macro"] = c.viaMacro
	}
	if c.isMacro {
		e.Meta["rust_macro"] = true
	}
}

// rustLastPathSegment reduces `tokio::spawn` to `spawn`.
func rustLastPathSegment(s string) string {
	if i := strings.LastIndex(s, "::"); i >= 0 {
		return s[i+2:]
	}
	return s
}
