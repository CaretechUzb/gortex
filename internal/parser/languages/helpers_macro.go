package languages

import (
	"regexp"
	"strings"
	"sync"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/parser"
	sitter "github.com/zzet/gortex/internal/parser/tsitter"
	cgrammar "github.com/zzet/gortex/internal/parser/tsitter/c"
	cppgrammar "github.com/zzet/gortex/internal/parser/tsitter/cpp"
)

// cMacroCallRe matches a call-like invocation `name(` inside a macro's
// replacement list. The captured identifier is the (possibly hidden)
// callee — write_log in `#define LOG(m) write_log(m)`.
var cMacroCallRe = regexp.MustCompile(`([A-Za-z_]\w*)\s*\(`)

// cKeywordsInMacroBody are C/C++ keywords that can syntactically precede
// a `(` in a replacement list but are never call targets.
var cKeywordsInMacroBody = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true, "return": true,
	"sizeof": true, "alignof": true, "_Alignof": true, "defined": true,
	"do": true, "else": true, "case": true, "static_cast": true,
	"reinterpret_cast": true, "const_cast": true, "dynamic_cast": true,
	"typeof": true, "decltype": true, "catch": true,
}

// emitCMacro emits a KindMacro node for a preproc_def / preproc_function_def
// node and, for function-like macros, the EdgeCalls its replacement list
// hides. defNode is the whole preproc_(function_)def node; isFunc selects
// the function-like shape (parameters + call recovery). lang is "c" or "cpp".
//
// The replacement list is a raw preproc_arg token (tree-sitter does not
// parse it as part of the enclosing file), so call recovery sub-parses
// the body with the C/C++ grammar and walks it for call_expression
// callees — plain `f()`, member `(o)->run()`, and qualified `ns::f()` —
// excluding the macro's own parameters and C/C++ keywords. A malformed
// body falls back to a regex scan. A call site like `SQ(2)` parses as an
// ordinary call_expression and already resolves against the macro by
// name, so caller -> macro -> body-call forms a two-hop path through the
// expansion.
// macroEndLine reports the 1-based line that last carries the definition's
// source. A preprocessor definition is terminated by its newline, and the
// grammar includes that newline in the node, so tree-sitter reports the end
// point at column 0 of the following row. Converting that row directly would
// extend the macro over the next declaration: a one-line `#define` above a
// function would own the function's own signature line, and every enclosing
// -symbol lookup on that line would answer with the macro.
func macroEndLine(defNode *sitter.Node) int {
	end := defNode.EndPoint()
	if end.Column == 0 && end.Row > defNode.StartPoint().Row {
		return int(end.Row)
	}
	return int(end.Row) + 1
}

func emitCMacro(defNode *sitter.Node, isFunc bool, filePath, fileID, lang string, src []byte, result *parser.ExtractionResult, seen map[string]bool) {
	if defNode == nil {
		return
	}
	var name, replacement string
	var params []string
	for i, _nc := 0, int(defNode.ChildCount()); i < _nc; i++ {
		c := defNode.Child(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "identifier":
			if name == "" {
				name = c.Content(src)
			}
		case "preproc_params":
			for j, _nc := 0, int(c.NamedChildCount()); j < _nc; j++ {
				p := c.NamedChild(j)
				if p != nil && p.Type() == "identifier" {
					params = append(params, p.Content(src))
				}
			}
		case "preproc_arg":
			replacement = strings.TrimSpace(c.Content(src))
		}
	}
	if name == "" {
		return
	}
	id := filePath + "::" + name
	if seen[id] {
		return
	}
	seen[id] = true

	line := int(defNode.StartPoint().Row) + 1
	macroKind := "object"
	if isFunc {
		macroKind = "function"
	}
	meta := map[string]any{"macro_kind": macroKind}
	if len(params) > 0 {
		meta["params"] = params
	}
	if replacement != "" {
		r := replacement
		if len(r) > macroBodyMaxLen {
			r = r[:macroBodyMaxLen]
		}
		meta["replacement"] = r
	}
	result.Nodes = append(result.Nodes, &graph.Node{
		ID: id, Kind: graph.KindMacro, Name: name,
		FilePath: filePath, StartLine: line, EndLine: macroEndLine(defNode),
		Language: lang, Meta: meta,
	})
	result.Edges = append(result.Edges, &graph.Edge{
		From: fileID, To: id, Kind: graph.EdgeDefines, FilePath: filePath, Line: line,
	})

	// Recover macro-hidden calls from the replacement list.
	if replacement == "" {
		return
	}
	paramSet := make(map[string]bool, len(params))
	for _, p := range params {
		paramSet[p] = true
	}
	callSeen := make(map[string]bool)
	for _, callee := range recoverMacroCallees(replacement, lang) {
		if paramSet[callee] || cKeywordsInMacroBody[callee] || callSeen[callee] {
			continue
		}
		callSeen[callee] = true
		result.Edges = append(result.Edges, &graph.Edge{
			From: id, To: "unresolved::" + callee,
			Kind: graph.EdgeCalls, FilePath: filePath, Line: line,
			Origin: graph.OriginASTInferred,
		})
	}
}

// macroBodyMaxLen bounds how much of a macro replacement list is both
// stored on the node and fed to the sub-parser. Macro bodies are almost
// always short; a body longer than this is pathological (e.g. a
// generated table) and is truncated for storage and scanned with the
// linear regex fallback rather than the tree-sitter parser, keeping
// recovery cheap and bounded.
const macroBodyMaxLen = 4096

// macroWrapPrefix / macroWrapSuffix wrap a replacement list in a minimal
// function body so an expression / statement fragment parses as a
// well-formed C/C++ translation unit. The leading newline before the
// closing `;}` keeps a trailing `//` line comment in the body from
// swallowing the terminator. Recovered names are read from the wrapped
// source, so the wrapper text never leaks into a callee name and no byte
// offset translation is needed.
const (
	macroWrapPrefix = "void __gx_macro_probe(void){\n"
	macroWrapSuffix = "\n;}"
)

// cMacroLang / cppMacroLang lazily build (once) the grammars used to
// sub-parse a macro body. GetLanguage allocates a fresh wrapper per call,
// so caching avoids an allocation on every function-like macro.
var (
	cMacroLang   = sync.OnceValue(cgrammar.GetLanguage)
	cppMacroLang = sync.OnceValue(cppgrammar.GetLanguage)
)

// recoverMacroCallees returns the callee names hidden in a macro
// replacement list, in source order. It prefers a real tree-sitter
// sub-parse of the body (which recovers member calls `(o)->run()` and
// qualified calls `ns::f()` the regex cannot see) and falls back to the
// regex scan whenever the body cannot be parsed cleanly — a parse
// failure or any ERROR/MISSING node — so a malformed body never recovers
// fewer calls than the historical regex behaviour. Pathologically long
// bodies skip the parser and use the linear regex directly.
func recoverMacroCallees(replacement, lang string) []string {
	if len(replacement) > macroBodyMaxLen {
		return regexMacroCallees(replacement)
	}
	if names, ok := subparseMacroCallees(replacement, lang); ok {
		return names
	}
	return regexMacroCallees(replacement)
}

// regexMacroCallees is the historical scan: every `name(` invocation in
// the body, in order, before parameter / keyword filtering (applied by
// the caller).
func regexMacroCallees(replacement string) []string {
	matches := cMacroCallRe.FindAllStringSubmatch(replacement, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

// subparseMacroCallees parses the macro body with the C or C++ grammar
// (selected by the macro's file language) and walks the tree for
// call_expression callees. It returns (names, true) only on a clean
// parse; a parse error, an ERROR/MISSING node anywhere in the tree, or a
// panic from the parser yields (nil, false) so the caller falls back to
// the regex. parser.ParseFile owns parser-pool safety: it Closes a
// parser whose parse errored rather than recycling it, so a malformed
// body cannot poison a pooled parser.
func subparseMacroCallees(replacement, lang string) (names []string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			names, ok = nil, false
		}
	}()
	grammar := cMacroLang()
	if lang == "cpp" {
		grammar = cppMacroLang()
	}
	wrapped := []byte(macroWrapPrefix + replacement + macroWrapSuffix)
	tree, err := parser.ParseFile(wrapped, grammar)
	if err != nil {
		return nil, false
	}
	defer tree.Close()
	root := tree.RootNode()
	if root == nil || root.HasError() {
		return nil, false
	}
	walkMacroCalls(root, wrapped, &names)
	return names, true
}

// walkMacroCalls appends the callee name of every call_expression in the
// subtree, in source order, descending into arguments so nested calls
// (`f(g())`) are all recovered.
func walkMacroCalls(n *sitter.Node, src []byte, out *[]string) {
	if n == nil {
		return
	}
	if n.Type() == "call_expression" {
		if name := macroCalleeName(n, src); name != "" {
			*out = append(*out, name)
		}
	}
	for c := range n.NamedChildren() {
		walkMacroCalls(c, src, out)
	}
}

// macroCalleeName recovers the callee name from a call_expression's
// `function` child across the three shapes a macro body can hide it in:
// a plain identifier (`f()`), a field access (`(o)->run()` / `o.run()`),
// or a qualified name (`ns::f()`).
func macroCalleeName(call *sitter.Node, src []byte) string {
	fn := call.ChildByFieldName("function")
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier":
		return fn.Content(src)
	case "field_expression":
		if field := fn.ChildByFieldName("field"); field != nil {
			return field.Content(src)
		}
	case "qualified_identifier":
		return rightmostQualifiedName(fn, src)
	}
	return ""
}

// rightmostQualifiedName returns the final segment of a (possibly nested)
// qualified_identifier — `deep` for `A::B::deep`, `f` for `ns::f`.
func rightmostQualifiedName(q *sitter.Node, src []byte) string {
	cur := q
	for cur != nil && cur.Type() == "qualified_identifier" {
		cur = cur.ChildByFieldName("name")
	}
	if cur != nil && (cur.Type() == "identifier" || cur.Type() == "field_identifier") {
		return cur.Content(src)
	}
	return ""
}
