package resolver

import "github.com/zzet/gortex/internal/graph"

// isCSharpExtension reports whether n is a C# extension method (a static method
// whose first parameter carries the `this` modifier). Such methods are bound
// only by the type-directed extension rule, never by the locality fallback. The
// Language check keeps this C#-only: other languages (e.g. Scala) also stamp
// Meta["extension"], and their locality resolution must be left unchanged.
func isCSharpExtension(n *graph.Node) bool {
	if n == nil || n.Language != "csharp" || n.Meta == nil {
		return false
	}
	v, _ := n.Meta["extension"].(bool)
	return v
}

// csharpHasCompetingMethod reports whether a non-extension method of the same
// name is among the candidates. C# resolves an instance/interface member over
// an extension, so without receiver-type evidence the extension must not
// preempt a competing member the locality fallback would otherwise bind.
func csharpHasCompetingMethod(candidates []*graph.Node) bool {
	for _, c := range candidates {
		if c != nil && c.Kind == graph.KindMethod && !isCSharpExtension(c) {
			return true
		}
	}
	return false
}

// tryBindCSharpExtension binds a failed C# member call `x.Foo(...)` to a static
// extension method `Foo(this X x)`. It runs after the receiver-type passes (an
// instance or interface member always wins over an extension in C#) and before
// the locality fallback. Candidates are the raw same-name in-repo nodes, so a
// reachability drop cannot hide a valid extension. Returns true when it binds.
//
// Precision rules — never guess on ambiguity, which would recreate the
// same-name-wrong-type misattribution the receiver-type gate exists to prevent:
//   - with receiver-type evidence: bind when exactly one extension's
//     this_param_type matches the receiver; a multi-way tie falls to the
//     visibility narrowing below before staying unresolved.
//   - without a matching type: bind when namespace visibility narrows the
//     same-name extensions to exactly one, else when the name maps to exactly
//     one extension method in the repo; otherwise stay unresolved.
//
// Visibility mirrors C#'s extension lookup: an extension is callable only when
// its namespace is an enclosing namespace of the call site or imported by a
// using directive — csharpFileNamespaceSet holds both. Narrowing only, never a
// loss: when no candidate namespace is visible (partial using data), the
// pre-existing rules run over the full set.
func (r *Resolver) tryBindCSharpExtension(e *graph.Edge, methodName, receiverType string, candidates []*graph.Node, stats *ResolveStats) bool {
	// C#-only: a non-C# caller must never bind to a C# extension method even
	// when a same-named one exists in a mixed-language repo.
	if cn := r.cachedGetNode(e.From); cn == nil || cn.Language != "csharp" {
		return false
	}
	var exts []*graph.Node
	for _, c := range candidates {
		if isCSharpExtension(c) {
			exts = append(exts, c)
		}
	}
	if len(exts) == 0 {
		return false
	}

	// With receiver-type evidence, prefer the extension whose this_param_type
	// matches the receiver. Exactly one match binds; more than one is an
	// overload/ambiguity we refuse to guess on.
	if receiverType != "" {
		var typed []*graph.Node
		for _, c := range exts {
			if tp, _ := c.Meta["this_param_type"].(string); tp != "" && tp == receiverType {
				typed = append(typed, c)
			}
		}
		if len(typed) == 1 {
			r.bindCSharpExtension(e, typed[0], 0.9, stats)
			return true
		}
		if len(typed) > 1 {
			// Same receiver type in several namespaces (the per-module
			// DI-configuration pattern) — the call site's visible
			// namespaces break the tie the type alone cannot.
			if vis := r.narrowCSharpExtensionsByVisibility(e, typed); len(vis) == 1 {
				r.bindCSharpExtension(e, vis[0], 0.9, stats)
				return true
			}
			return false
		}
	}

	// No type evidence (or no typed match): visibility narrowing first, then
	// the repo-unique name. Either way a non-extension member of the same name
	// blocks the bind (C# instance-method precedence — let the locality
	// fallback bind the instance method instead).
	pool := exts
	if vis := r.narrowCSharpExtensionsByVisibility(e, exts); len(vis) == 1 {
		pool = vis
	}
	if len(pool) == 1 && !csharpHasCompetingMethod(candidates) {
		r.bindCSharpExtension(e, pool[0], 0.75, stats)
		return true
	}
	return false
}

// narrowCSharpExtensionsByVisibility filters same-name extension candidates to
// those whose scope_ns the calling file can see — enclosing namespaces first
// (deepest wins, matching member-lookup order), then using-directive imports.
// Same tiering as csharpNarrowByNamespace's type narrowing, over method nodes.
// Returns nil when no candidate namespace is visible: narrowing only, never a
// loss.
func (r *Resolver) narrowCSharpExtensionsByVisibility(e *graph.Edge, exts []*graph.Node) []*graph.Node {
	if len(exts) < 2 {
		return nil
	}
	visible := r.csharpFileNamespaceSet(e.FilePath)
	if len(visible.enclosing) == 0 && len(visible.imported) == 0 {
		return nil
	}
	var enclosing, imported []*graph.Node
	deepest := 0
	for _, c := range exts {
		ns, _ := c.Meta["scope_ns"].(string)
		if ns == "" {
			continue
		}
		if _, ok := visible.enclosing[ns]; ok {
			switch {
			case len(ns) > deepest:
				enclosing, deepest = []*graph.Node{c}, len(ns)
			case len(ns) == deepest:
				enclosing = append(enclosing, c)
			}
			continue
		}
		if _, ok := visible.imported[ns]; ok {
			imported = append(imported, c)
		}
	}
	if len(enclosing) > 0 {
		return enclosing
	}
	return imported
}

// bindCSharpExtension points a member-call edge at a resolved extension method
// at the ast_inferred tier — the binding is type-directed but not compiler-
// verified (extension visibility depends on `using` scope we do not fully model).
func (r *Resolver) bindCSharpExtension(e *graph.Edge, target *graph.Node, conf float64, stats *ResolveStats) {
	e.To = target.ID
	e.Origin = graph.OriginASTInferred
	e.Confidence = conf
	e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, conf)
	if e.Meta == nil {
		e.Meta = map[string]any{}
	}
	e.Meta["resolution"] = "extension_method"
	stats.Resolved++
}
