package resolver

import (
	"github.com/zzet/gortex/internal/graph"
)

// C# applicability narrowing — the arity half of overload resolution.
//
// Receiver-type evidence separates extensions that extend DIFFERENT
// types. It says nothing about a set that extends the SAME type, which
// is the dominant .NET shape: `AddFoo(this IServiceCollection)` beside
// `AddFoo(this IServiceCollection, Action<Options>)`. Namespace
// visibility cannot break that tie either — sibling overloads almost
// always share a namespace — so the binder's ambiguity refusal used to
// drop every such call, leaving the declaration looking uncalled.
//
// C# resolves those by APPLICABILITY: a candidate is in the running only
// if the call's argument count fits its parameter list and its type-
// parameter count matches any explicitly spelled type arguments. Both
// are exact, spec-grounded rules — not heuristics — so applying them
// before the visibility tie-break both matches §12.8.10.3 (each scope
// considers only APPLICABLE candidates) and turns the common overload
// pair into a single answer.
//
// Every function here narrows only. Missing evidence, an unparseable
// stamp, or a filter that would empty the set all return the input
// untouched: this can turn a refusal into a bind, never a bind into a
// different bind.
//
// The arity window matches ResolveCppOverload's cppArityCompatible — the
// same [required, declared] range widened by a variadic tail. The two
// stay separate because C++ ranks on parameter TYPES as well, and C#
// alone has to discount the receiver's `this` slot.

// csharpNarrowByApplicability filters a same-name C# candidate set to the
// members the call site could actually invoke.
func csharpNarrowByApplicability(e *graph.Edge, cands []*graph.Node) []*graph.Node {
	if len(cands) < 2 || e == nil || e.Meta == nil {
		return cands
	}
	out := csharpKeepIf(cands, func(c *graph.Node) bool {
		return csharpAcceptsTypeArgCount(e, c)
	})
	return csharpKeepIf(out, func(c *graph.Node) bool {
		return csharpExtensionAcceptsArgCount(e, c)
	})
}

// csharpKeepIf applies one narrowing predicate. An empty result is
// discarded — a rule that excludes everything is evidence the rule
// misread the code, not evidence the call is unresolvable.
func csharpKeepIf(cands []*graph.Node, keep func(*graph.Node) bool) []*graph.Node {
	if len(cands) < 2 {
		return cands
	}
	out := make([]*graph.Node, 0, len(cands))
	for _, c := range cands {
		if c != nil && keep(c) {
			out = append(out, c)
		}
	}
	if len(out) == 0 || len(out) == len(cands) {
		return cands
	}
	return out
}

// csharpAcceptsTypeArgCount reports whether a candidate's own generic
// arity matches the type arguments the call spells explicitly. A call
// with no explicit type arguments proves nothing (C# infers them), and a
// candidate with no type-parameter stamp is treated as non-generic.
func csharpAcceptsTypeArgCount(e *graph.Edge, c *graph.Node) bool {
	want, ok := metaIntValue(e.Meta["type_arg_count"])
	if !ok {
		return true
	}
	return csharpMethodTypeParamCount(c) == want
}

// csharpMethodTypeParamCount counts a method's own type parameters from
// the extractor's comma-joined stamp.
func csharpMethodTypeParamCount(c *graph.Node) int {
	if c == nil || c.Meta == nil {
		return 0
	}
	s, _ := c.Meta["method_type_params"].(string)
	if s == "" {
		return 0
	}
	n := 1
	for _, r := range s {
		if r == ',' {
			n++
		}
	}
	return n
}

// csharpExtensionAcceptsArgCount reports whether a candidate extension
// method can accept the call's argument count written in EXTENSION FORM
// (`x.Foo(a)`), where the receiver supplies the `this` parameter.
//
// The window is [required, count], widened to [required, +inf) by a
// trailing `params` array. A candidate with no parameter stamp — an
// older graph, or a declaration whose parameter list the grammar did not
// expose — accepts anything.
func csharpExtensionAcceptsArgCount(e *graph.Edge, c *graph.Node) bool {
	argc, ok := metaIntValue(e.Meta["arg_count"])
	if !ok || c == nil || c.Meta == nil {
		return true
	}
	count, ok := metaIntValue(c.Meta["param_count"])
	if !ok || count == 0 {
		// An extension method always declares at least the `this`
		// parameter, so a zero here means "no evidence", not "takes
		// nothing".
		return true
	}
	required := count
	if r, rok := metaIntValue(c.Meta["param_required"]); rok {
		required = r
	}
	// The receiver fills the `this` slot; the remaining parameters are
	// what the argument list has to cover.
	count--
	required--
	if required < 0 {
		required = 0
	}
	if variadic, _ := c.Meta["param_variadic"].(bool); variadic {
		return argc >= required
	}
	return argc >= required && argc <= count
}
