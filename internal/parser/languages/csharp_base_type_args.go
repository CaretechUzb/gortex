package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// Generic type arguments (interface-dispatch precision).
//
// A class implementing IBoxStore<Crate> and a class implementing
// IBoxStore<Widget> implement DIFFERENT constructed interfaces, but the
// implements edges both target the erased IBoxStore — so the
// interface-dispatch fan-out treats every implementor as one family and
// fans an IBoxStore<Crate> receiver's calls into the Widget impl. On a
// per-entity-repository codebase (one generic repository interface with
// a hundred-plus implementors) that inflates every data-access usage
// answer.
//
// Two stamps carry the CLOSED arguments as evidence:
//
//   - implements/extends edges: Meta["target_type_args"] from the
//     base-list entry (emitCSharpBaseList);
//   - field/property nodes: Meta["field_type_args"] from the declared
//     type (emitField/emitProperty) — the dispatch pass reads the stamp
//     instead of re-parsing field_type text, so the open/closed rules
//     live in exactly one place: here, where the enclosing-type chain
//     is visible.
//
// The rules are deliberately conservative — absence means "do not
// filter", never "no arguments":
//
//   - an argument that names a type parameter of the declaring type OR
//     of ANY enclosing type (class Outer<T> { class Inner : IBoxStore<T> })
//     closes nothing — skip;
//   - a non-simple argument (nested generic, array, nullable, tuple)
//     is beyond the resolver's cheap string comparison — skip;
//   - a qualified argument (App.Crate) normalizes to Crate, the same
//     last-segment convention resolver-side type names already use;
//   - a base list that names the SAME erased target twice
//     (Both : IBoxStore<Crate>, IBoxStore<Widget>) collapses to one
//     stored edge, so the ambiguity would be invisible downstream —
//     neither closure stamps (guarded in emitCSharpBaseList);
//   - a qualified base whose generic segment is not the FINAL one
//     (Outer<int>.IInner) never stamps the outer segment's arguments.

// csharpFileAliasNames collects every using-alias identifier the FILE
// declares, in one walk, regardless of scope. An alias may spell any type,
// including one whose canonical form differs from the alias identifier, so
// it is opaque to a string comparison — and treating every alias as
// in-scope everywhere in the file over-refuses at worst, which only ever
// PRESERVES fan-out edges. Computed once per extraction and threaded to
// each stamp site: the previous per-declaration ancestor scan re-walked
// the enclosing namespace's whole declaration list, quadratic in sibling
// count (the review's 2,000-sibling fixture).
func csharpFileAliasNames(root *sitter.Node, src []byte) map[string]bool {
	var out map[string]bool
	walkNodes(root, func(n *sitter.Node) {
		if name := csharpUsingAliasName(n, src); name != "" {
			if out == nil {
				out = map[string]bool{}
			}
			out[name] = true
		}
	})
	return out
}

// csharpUnstampableArgNames collects every identifier that must NOT be
// read as a closed concrete type argument at node's position:
//
//   - type parameters of every enclosing type declaration, the node's own
//     included — a nested type legitimately closes over its outer types'
//     parameters, and every one of them is open (per-declaration ancestor
//     walk, cheap);
//   - the file's using-alias names, precollected by csharpFileAliasNames.
//
// Both categories mean the same thing to the caller: this spelling does
// not denote a type the dispatch gate may compare by name.
func csharpUnstampableArgNames(node *sitter.Node, src []byte, fileAliases map[string]bool) map[string]bool {
	var out map[string]bool
	add := func(name string) {
		if name == "" {
			return
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[name] = true
	}
	for n := node; n != nil; n = n.Parent() {
		switch n.Type() {
		case "class_declaration", "struct_declaration", "record_declaration", "interface_declaration":
			for name := range csharpMethodTypeParamNames(n, src) {
				add(name)
			}
		}
	}
	for name := range fileAliases {
		add(name)
	}
	return out
}

// csharpHasVariantTypeParams reports whether decl's type-parameter list
// declares any `in`/`out` variance modifier. A variance-declaring
// interface makes differently-closed constructions assignable across the
// implements family (ISource<Dog> satisfies an ISource<out T> receiver
// closed over Animal), so the dispatch gate — which models invariant
// parameters only — must never arm for it.
func csharpHasVariantTypeParams(decl *sitter.Node) bool {
	if decl == nil {
		return false
	}
	tparams := decl.ChildByFieldName("type_parameters")
	if tparams == nil {
		for i, _nc := 0, int(decl.NamedChildCount()); i < _nc; i++ {
			c := decl.NamedChild(i)
			if c != nil && c.Type() == "type_parameter_list" {
				tparams = c
				break
			}
		}
	}
	if tparams == nil {
		return false
	}
	for i, _nc := 0, int(tparams.NamedChildCount()); i < _nc; i++ {
		tp := tparams.NamedChild(i)
		if tp == nil || tp.Type() != "type_parameter" {
			continue
		}
		// The variance keyword is an anonymous token child of the
		// type_parameter, before its identifier.
		for j, _jc := 0, int(tp.ChildCount()); j < _jc; j++ {
			if c := tp.Child(j); c != nil && (c.Type() == "in" || c.Type() == "out") {
				return true
			}
		}
	}
	return false
}

// csharpUsingAliasName returns the alias identifier a using directive
// introduces (`using MyCrate = App.Crate;` → "MyCrate"), or "" when the
// node is not an alias directive. Grammar revisions differ — some wrap
// the alias in a name_equals node, others lay it out flat (identifier,
// bare `=` token, target); stampCSharpUsings' skip branch matches the
// same pair.
func csharpUsingAliasName(n *sitter.Node, src []byte) string {
	if n == nil || n.Type() != "using_directive" {
		return ""
	}
	firstIdent := ""
	for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
		c := n.Child(i)
		if c == nil {
			continue
		}
		switch c.Type() {
		case "name_equals":
			for j, _jc := 0, int(c.NamedChildCount()); j < _jc; j++ {
				if id := c.NamedChild(j); id != nil && id.Type() == "identifier" {
					return strings.TrimSpace(id.Content(src))
				}
			}
		case "=":
			return firstIdent
		case "identifier":
			if firstIdent == "" {
				firstIdent = strings.TrimSpace(c.Content(src))
			}
		}
	}
	return ""
}

// csharpCanonicalTypeArg folds the BCL alias spellings of the C# built-in
// types onto their keyword form, so `System.Int32`, `Int32` and `int` —
// the SAME constructed type — compare equal on both sides of the gate.
// Mirrors the resolver's own csharpTypeSuffixTrim fold; kept local
// because the parser package must not depend on the resolver.
func csharpCanonicalTypeArg(t string) string {
	switch t {
	case "String":
		return "string"
	case "Boolean":
		return "bool"
	case "Byte":
		return "byte"
	case "SByte":
		return "sbyte"
	case "Char":
		return "char"
	case "Decimal":
		return "decimal"
	case "Double":
		return "double"
	case "Single":
		return "float"
	case "Int16":
		return "short"
	case "UInt16":
		return "ushort"
	case "Int32":
		return "int"
	case "UInt32":
		return "uint"
	case "Int64":
		return "long"
	case "UInt64":
		return "ulong"
	case "Object":
		return "object"
	case "dynamic":
		// dynamic erases to object — the two spellings construct over the
		// same underlying type, and folding can only CREATE matches.
		return "object"
	case "IntPtr":
		return "nint"
	case "UIntPtr":
		return "nuint"
	}
	return t
}

// csharpBaseTypeArgs returns the comma-joined, normalized type-argument
// list of a generic base-list entry, or "" when the entry is not generic
// or any argument is open or non-simple. openParams is the full
// enclosing-chain parameter set (csharpEnclosingTypeParams).
func csharpBaseTypeArgs(entry *sitter.Node, src []byte, openParams map[string]bool) string {
	argList := csharpEntryTypeArgumentList(entry)
	if argList == nil {
		return ""
	}
	var args []string
	for i, _nc := 0, int(argList.NamedChildCount()); i < _nc; i++ {
		arg := argList.NamedChild(i)
		if arg == nil {
			continue
		}
		norm := csharpNormalizeSimpleArg(arg.Content(src), openParams)
		if norm == "" {
			return ""
		}
		args = append(args, norm)
	}
	if len(args) == 0 {
		return ""
	}
	return strings.Join(args, ",")
}

// csharpNormalizeSimpleArg reduces one type-argument spelling to its
// comparable form, or "" when it is non-simple or names an open
// parameter.
func csharpNormalizeSimpleArg(text string, openParams map[string]bool) string {
	text = strings.TrimSpace(text)
	if text == "" || strings.ContainsAny(text, "<[?(, ") {
		// Nested generic, array, nullable, tuple, or anything else
		// beyond one identifier chain — not comparable by string.
		return ""
	}
	if strings.ContainsAny(text, "/*") {
		// Comment trivia is legal between the tokens of a qualified
		// spelling (`App./**/Crate`) and no part of type identity — raw
		// text carrying it must never be compared as a spelling.
		return ""
	}
	// Qualifiers reduce to the final segment — `global::App.Crate`, an
	// extern-alias qualifier, and a dotted namespace all name the same
	// last-segment type the resolver-side convention compares by.
	if i := strings.LastIndex(text, "::"); i >= 0 {
		text = text[i+2:]
	}
	if dot := strings.LastIndex(text, "."); dot >= 0 {
		text = text[dot+1:]
	}
	// A verbatim identifier (`@Crate`, `@T`) names the same symbol as its
	// bare spelling. Strip BEFORE the open-parameter/alias check so
	// `IBox<@T>` reads as the open parameter T — never as a closed type
	// spelled "@T" that would gate the open implementor out.
	text = strings.TrimPrefix(text, "@")
	if text == "" || openParams[text] {
		return ""
	}
	// Fold AFTER the open-name check: a type parameter or alias named
	// like a CLR type is caught above; a genuine BCL spelling folds to
	// the keyword so both sides of the gate agree. A user type that
	// happens to be named Int32 folds too — harmless, because folding
	// can only CREATE matches and a match always keeps the edge.
	return csharpCanonicalTypeArg(text)
}

// csharpTypeArgsFromTypeNode is csharpBaseTypeArgs over a declared-type
// AST node (a field/property/positional-parameter type):
// "IBoxStore<Crate>" → "Crate". The arguments come from the parsed tree,
// not from raw source text, so comment trivia between tokens
// (`IBox</**/Crate>`) never becomes part of the compared spelling — the
// earlier text-based path stamped "/**/Crate" as an argument and the
// dispatch gate then filtered the valid implementor. "" when the type is
// not a plain generic name (wrapped forms — nullable, array — refuse the
// same way the text path did), or any argument is open or non-simple.
func csharpTypeArgsFromTypeNode(typeNode *sitter.Node, src []byte, openParams map[string]bool) string {
	if typeNode == nil {
		return ""
	}
	switch typeNode.Type() {
	case "generic_name", "qualified_name":
	default:
		return ""
	}
	argList := csharpEntryTypeArgumentList(typeNode)
	if argList == nil {
		return ""
	}
	var args []string
	for i, _nc := 0, int(argList.NamedChildCount()); i < _nc; i++ {
		arg := argList.NamedChild(i)
		if arg == nil || arg.Type() == "comment" {
			continue
		}
		norm := csharpNormalizeSimpleArg(arg.Content(src), openParams)
		if norm == "" {
			return ""
		}
		args = append(args, norm)
	}
	if len(args) == 0 {
		return ""
	}
	return strings.Join(args, ",")
}

// csharpEntryTypeArgumentList returns the base entry's OWN
// type_argument_list: the final name segment's, and only when it is the
// single one in the whole entry. `Outer<int>.IInner` carries a list on a
// non-final segment — its arguments belong to Outer, never to the edge's
// target IInner — and `Outer<int>.IInner<string>` is beyond the cheap
// comparison entirely; both answer nil (no stamp).
func csharpEntryTypeArgumentList(entry *sitter.Node) *sitter.Node {
	if entry == nil {
		return nil
	}
	if csharpCountTypeArgumentLists(entry) != 1 {
		return nil
	}
	// Descend to the final name segment of a qualified spelling.
	final := entry
	for final.Type() == "qualified_name" {
		next := final.ChildByFieldName("name")
		if next == nil {
			return nil
		}
		final = next
	}
	if final.Type() != "generic_name" {
		return nil
	}
	for i, _nc := 0, int(final.ChildCount()); i < _nc; i++ {
		if c := final.Child(i); c != nil && c.Type() == "type_argument_list" {
			return c
		}
	}
	return nil
}

// csharpCountTypeArgumentLists counts every type_argument_list in the
// subtree.
func csharpCountTypeArgumentLists(n *sitter.Node) int {
	if n == nil {
		return 0
	}
	count := 0
	if n.Type() == "type_argument_list" {
		count++
	}
	for i, _nc := 0, int(n.ChildCount()); i < _nc; i++ {
		count += csharpCountTypeArgumentLists(n.Child(i))
	}
	return count
}
