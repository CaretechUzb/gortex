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

// csharpUnstampableArgNames collects every identifier that must NOT be
// read as a closed concrete type argument at node's position, walking the
// ancestor chain once:
//
//   - type parameters of every enclosing type declaration, the node's own
//     included — a nested type legitimately closes over its outer types'
//     parameters, and every one of them is open;
//   - using-alias names in scope (`using MyCrate = App.Crate;`, at file
//     level or inside an enclosing namespace) — an alias may spell any
//     type, including one whose canonical form differs from the alias
//     identifier, so it is opaque to a string comparison.
//
// Both categories mean the same thing to the caller: this spelling does
// not denote a type the dispatch gate may compare by name.
func csharpUnstampableArgNames(node *sitter.Node, src []byte) map[string]bool {
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
		case "compilation_unit", "namespace_declaration", "file_scoped_namespace_declaration":
			// Using directives are DIRECT children of the compilation
			// unit or of a namespace declaration (a block namespace
			// keeps its members one level down, in declaration_list) —
			// all of which sit on the ancestor chain of any type, so
			// this shallow scan sees every alias actually in scope,
			// scoped usings included, without a whole-tree walk.
			for i, _nc := 0, int(n.NamedChildCount()); i < _nc; i++ {
				c := n.NamedChild(i)
				if c != nil && c.Type() == "declaration_list" {
					for j, _jc := 0, int(c.NamedChildCount()); j < _jc; j++ {
						add(csharpUsingAliasName(c.NamedChild(j), src))
					}
					continue
				}
				add(csharpUsingAliasName(c, src))
			}
		}
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
	if dot := strings.LastIndex(text, "."); dot >= 0 {
		text = text[dot+1:]
	}
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

// csharpSimpleTypeArgsFromText is csharpBaseTypeArgs over a declared-type
// TEXT (a field/property's type spelling): "IBoxStore<Crate>" → "Crate".
// "" when the text is not generic, the argument section is non-simple,
// or any argument is an open parameter.
func csharpSimpleTypeArgsFromText(text string, openParams map[string]bool) string {
	text = strings.TrimSpace(text)
	lt := strings.Index(text, "<")
	if lt <= 0 || !strings.HasSuffix(text, ">") {
		return ""
	}
	inner := text[lt+1 : len(text)-1]
	if inner == "" || strings.ContainsAny(inner, "<[?(") {
		return ""
	}
	rawArgs := strings.Split(inner, ",")
	args := make([]string, 0, len(rawArgs))
	for _, a := range rawArgs {
		norm := csharpNormalizeSimpleArg(a, openParams)
		if norm == "" {
			return ""
		}
		args = append(args, norm)
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
