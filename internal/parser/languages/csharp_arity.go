package languages

import (
	"strings"

	sitter "github.com/zzet/gortex/internal/parser/tsitter"
)

// C# overload evidence — the shape facts that let the resolver pick one
// member out of a same-name set without guessing.
//
// A same-name C# method set is split by APPLICABILITY before anything
// else: how many arguments the call passes, and how many type arguments
// it spells. Receiver type alone cannot separate overloads that all
// extend the same type (`AddFoo(this IServiceCollection)` vs
// `AddFoo(this IServiceCollection, Action<Options>)`), which is the
// dominant shape in .NET DI registration code. Without these stamps the
// resolver's ambiguity refusal drops the call edge entirely.
//
// Both sides of the comparison are recorded here: the call site's
// argument / type-argument counts (stamped on the EdgeCalls) and the
// declaration's parameter arity (stamped on the method node).

// csharpCallArgCount counts the argument expressions of an invocation.
// ok=false when the node carries no argument list at all, so an unknown
// arity is never mistaken for a zero-argument call.
func csharpCallArgCount(inv *sitter.Node) (int, bool) {
	if inv == nil {
		return 0, false
	}
	args := inv.ChildByFieldName("arguments")
	if args == nil {
		return 0, false
	}
	n := 0
	for i, nc := 0, int(args.NamedChildCount()); i < nc; i++ {
		// Named children include comments; only `argument` nodes count.
		if c := args.NamedChild(i); c != nil && c.Type() == "argument" {
			n++
		}
	}
	return n, true
}

// csharpCallTypeArgCount counts the EXPLICIT type arguments an
// invocation spells (`Foo<A, B>()` → 2). ok=false when the invoked name
// carries no type-argument list — C# infers them there, so the count is
// no evidence about the callee's arity.
func csharpCallTypeArgCount(inv *sitter.Node) (int, bool) {
	if inv == nil {
		return 0, false
	}
	fn := inv.ChildByFieldName("function")
	if fn == nil {
		return 0, false
	}
	// `Foo<T>()` invokes a generic_name directly; `x.Foo<T>()` and
	// `x?.Foo<T>()` wrap it in the member access / binding's name field.
	switch fn.Type() {
	case "member_access_expression", "member_binding_expression":
		fn = fn.ChildByFieldName("name")
	case "conditional_access_expression":
		fn = csharpFirstChildOfType(fn, "member_binding_expression")
		if fn != nil {
			fn = fn.ChildByFieldName("name")
		}
	}
	if fn == nil || fn.Type() != "generic_name" {
		return 0, false
	}
	list := csharpFirstChildOfType(fn, "type_argument_list")
	if list == nil {
		return 0, false
	}
	n := 0
	for i, nc := 0, int(list.NamedChildCount()); i < nc; i++ {
		if c := list.NamedChild(i); c != nil && c.Type() != "comment" {
			n++
		}
	}
	return n, n > 0
}

// csharpParamEntry is the normalized signature fact shared by arity stamps and
// function-shape nodes. It represents both ordinary `parameter` nodes and the
// vendored grammar's flattened `params` form.
type csharpParamEntry struct {
	name       string
	typeRaw    string
	variadic   bool
	hasDefault bool
	position   int
	startLine  int
	endLine    int
}

// csharpParamEntries normalizes the two parameter shapes emitted by the
// vendored grammar. Ordinary entries are `parameter` nodes; a `params` entry
// is flattened into the anonymous `params` token followed by loose type and
// identifier siblings. Unknown shapes make complete=false so arity narrowing
// remains fail-open, while recognized entries can still supply shape nodes.
func csharpParamEntries(params *sitter.Node, src []byte) (entries []csharpParamEntry, complete bool) {
	if params == nil {
		return nil, false
	}
	complete = true
	segment := make([]*sitter.Node, 0, 3)
	declaredPos := 0
	flush := func() {
		if len(segment) == 0 {
			return
		}
		entry, ok := csharpParamEntryFromSegment(segment, src)
		if !ok {
			complete = false
		} else {
			entry.position = declaredPos
			entries = append(entries, entry)
		}
		declaredPos++
		segment = segment[:0]
	}
	for i, nc := 0, int(params.ChildCount()); i < nc; i++ {
		child := params.Child(i)
		if child == nil {
			continue
		}
		switch child.Type() {
		case "(", ")":
			flush()
		case ",":
			flush()
		case "comment":
			continue
		default:
			segment = append(segment, child)
		}
	}
	flush()
	return entries, complete
}

func csharpParamEntryFromSegment(segment []*sitter.Node, src []byte) (csharpParamEntry, bool) {
	if len(segment) == 1 && segment[0].Type() == "parameter" {
		param := segment[0]
		name := param.ChildByFieldName("name")
		typeNode := param.ChildByFieldName("type")
		if name == nil || typeNode == nil {
			return csharpParamEntry{}, false
		}
		return csharpParamEntry{
			name:       name.Content(src),
			typeRaw:    strings.TrimSpace(typeNode.Content(src)),
			variadic:   csharpParamIsVariadic(param, src),
			hasDefault: csharpParamHasDefault(param),
			startLine:  int(param.StartPoint().Row) + 1,
			endLine:    int(param.EndPoint().Row) + 1,
		}, true
	}

	// tree-sitter-c-sharp v0.23.5 hides `_parameter_array`, leaving exactly
	// `params`, the complete type subtree, and the bound identifier at list
	// level. Keep this strict so a grammar bump fails safely instead of
	// inventing arity from a partially understood segment.
	if len(segment) != 3 || segment[0].Content(src) != "params" || segment[2].Type() != "identifier" {
		return csharpParamEntry{}, false
	}
	typeRaw := strings.TrimSpace(segment[1].Content(src))
	name := segment[2].Content(src)
	if typeRaw == "" || name == "" {
		return csharpParamEntry{}, false
	}
	return csharpParamEntry{
		name:      name,
		typeRaw:   typeRaw,
		variadic:  true,
		startLine: int(segment[0].StartPoint().Row) + 1,
		endLine:   int(segment[2].EndPoint().Row) + 1,
	}, true
}

// csharpParamArity summarises a declaration's total parameter count, required
// count, and variadic upper bound. The extension receiver counts as an ordinary
// declaration slot; the consumer adjusts for extension-call form. Unknown
// grammar shapes return ok=false so overload narrowing remains fail-open.
func csharpParamArity(decl *sitter.Node, src []byte) (count, required int, variadic bool, ok bool) {
	if decl == nil {
		return 0, 0, false, false
	}
	params := decl.ChildByFieldName("parameters")
	if params == nil {
		return 0, 0, false, false
	}
	entries, complete := csharpParamEntries(params, src)
	if !complete {
		return 0, 0, false, false
	}
	for _, entry := range entries {
		count++
		if entry.variadic {
			variadic = true
		}
		// A defaulted parameter (`int retries = 3`) and a `params`
		// array are both omissible at the call site.
		if entry.variadic || entry.hasDefault {
			continue
		}
		required++
	}
	return count, required, variadic, true
}

// csharpParamHasDefault reports whether a parameter declares a default
// value. The grammar spells it as a bare `=` token followed by the value
// expression — there is no equals-value wrapper node to look for.
func csharpParamHasDefault(p *sitter.Node) bool {
	if p == nil {
		return false
	}
	for i, nc := 0, int(p.ChildCount()); i < nc; i++ {
		if c := p.Child(i); c != nil && c.Type() == "=" {
			return true
		}
	}
	return false
}

// csharpParamIsVariadic reports whether a parameter carries the `params`
// modifier — C#'s variadic marker. Both modifier spellings the grammar
// has used are accepted so the check survives a grammar bump.
func csharpParamIsVariadic(p *sitter.Node, src []byte) bool {
	if p == nil {
		return false
	}
	for i, nc := 0, int(p.NamedChildCount()); i < nc; i++ {
		c := p.NamedChild(i)
		if c == nil {
			continue
		}
		if c.Type() != "modifier" && c.Type() != "parameter_modifier" {
			continue
		}
		if strings.Contains(c.Content(src), "params") {
			return true
		}
	}
	return false
}
