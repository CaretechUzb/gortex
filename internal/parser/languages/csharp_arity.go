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

// csharpParamArity summarises a declaration's parameter list: the total
// declared count, how many a caller MUST supply, and whether a `params`
// array lifts the upper bound. The `this` parameter of an extension
// method is counted like any other — the consumer knows whether the call
// site writes extension form or static form and adjusts.
//
// ok=false when the list is absent OR when the grammar did not resolve
// every entry into a `parameter` node. The vendored C# grammar flattens
// a `params object[] rest` entry into loose `array_type` + `identifier`
// siblings instead of a parameter, so a naive count would report one
// parameter for a two-parameter method — and an overload narrowed on
// that count would be excluded for argument counts it actually accepts.
// A list we cannot read in full yields no evidence at all, which leaves
// the method universally applicable rather than wrongly excluded.
func csharpParamArity(decl *sitter.Node, src []byte) (count, required int, variadic bool, ok bool) {
	if decl == nil {
		return 0, 0, false, false
	}
	params := decl.ChildByFieldName("parameters")
	if params == nil {
		return 0, 0, false, false
	}
	for i, nc := 0, int(params.NamedChildCount()); i < nc; i++ {
		p := params.NamedChild(i)
		if p == nil || p.Type() == "comment" {
			continue
		}
		if p.Type() != "parameter" {
			return 0, 0, false, false
		}
		count++
		isVariadic := csharpParamIsVariadic(p, src)
		if isVariadic {
			variadic = true
		}
		// A defaulted parameter (`int retries = 3`) and a `params`
		// array are both omissible at the call site.
		if isVariadic || csharpParamHasDefault(p) {
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
