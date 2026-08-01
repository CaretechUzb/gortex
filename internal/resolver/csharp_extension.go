package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

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
	// Builtin receivers ride a separate stamp — kept out of
	// receiver_type so the receiver-gate passes stay keyed on user
	// types — but they are receiver evidence all the same.
	if receiverType == "" && e.Meta != nil {
		receiverType, _ = e.Meta["receiver_builtin"].(string)
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
		want := csharpExtTypeKey(receiverType)
		var typed []*graph.Node
		for _, c := range exts {
			if tp, _ := c.Meta["this_param_type"].(string); tp != "" && csharpExtTypeKey(tp) == want {
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
		// Receiver evidence that matches NO candidate contradicts them
		// all — visibility is not applicability, and falling through to
		// the untyped pool would let a visible same-name extension on an
		// unrelated type swallow e.g. a BCL instance call (`xs.Add(1)`).
		// Inheritance-compatible receivers are invisible to the exact
		// match and refuse too: a missing edge, never a wrong one.
		return false
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
	enclosingSet := r.csharpCallerEnclosing(e, visible)
	if len(enclosingSet) == 0 && len(visible.imported) == 0 && len(visible.statics) == 0 {
		return nil
	}
	var enclosing, imported []*graph.Node
	deepest := 0
	for _, c := range exts {
		ns, _ := c.Meta["scope_ns"].(string)
		if ns != "" {
			if _, ok := enclosingSet[ns]; ok {
				// Within the caller's own chain the namespaces are
				// nested prefixes, so string length orders exactly by
				// depth.
				switch {
				case len(ns) > deepest:
					enclosing, deepest = []*graph.Node{c}, len(ns)
				case len(ns) == deepest:
					enclosing = append(enclosing, c)
				}
				continue
			}
		}
		// `using static Ns.Class;` admits the class's extensions
		// directly, namespace visibility notwithstanding — ranked with
		// the imports, after the enclosing tier (inner scopes win).
		if len(visible.statics) > 0 {
			if cls, _ := c.Meta["receiver"].(string); cls != "" {
				fqn := cls
				if ns != "" {
					fqn = ns + "." + cls
				}
				if _, ok := visible.statics[fqn]; ok {
					imported = append(imported, c)
					continue
				}
			}
		}
		if ns == "" {
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

// csharpCallerEnclosing is the enclosing-namespace set for an extension
// lookup: the CALLING NODE's own scope_ns chain when it carries one —
// the file-level set unions every namespace block in the file, and a
// sibling block's deeper namespace must not out-rank the call site's
// own. Falls back to the file union for callers without the stamp
// (older graphs).
func (r *Resolver) csharpCallerEnclosing(e *graph.Edge, visible csharpFileNS) map[string]struct{} {
	if cn := r.cachedGetNode(e.From); cn != nil {
		if scope, _ := cn.Meta["scope_ns"].(string); scope != "" {
			set := map[string]struct{}{}
			for scope != "" {
				set[scope] = struct{}{}
				i := strings.LastIndex(scope, ".")
				if i < 0 {
					break
				}
				scope = scope[:i]
			}
			return set
		}
	}
	return visible.enclosing
}

// csharpExtTypeKey canonicalises a type name for extension-eligibility
// comparison. The two sides arrive differently normalised —
// this_param_type strips namespace + generics but keeps ?/[]; the
// call-site receiver_type keeps qualification and strips suffixes — so
// compare on the shared core: last namespace segment, no suffixes.
func csharpExtTypeKey(t string) string {
	t = strings.TrimSuffix(strings.TrimSpace(t), "?")
	if i := strings.Index(t, "["); i > 0 {
		t = t[:i]
	}
	if i := strings.Index(t, "<"); i > 0 {
		t = t[:i]
	}
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// csharpExtensionVisible reports whether an extension method's declaring
// namespace is visible from the call site — via the caller's enclosing-
// namespace chain, a using directive (project-scoped globals included),
// or a using-static of the declaring class. Same evidence the narrowing
// uses, so the guard keep-rule and the bind can never disagree.
func (r *Resolver) csharpExtensionVisible(e *graph.Edge, fileID string, c *graph.Node) bool {
	visible := r.csharpFileNamespaceSet(fileID)
	ns, _ := c.Meta["scope_ns"].(string)
	if cls, _ := c.Meta["receiver"].(string); cls != "" && len(visible.statics) > 0 {
		fqn := cls
		if ns != "" {
			fqn = ns + "." + cls
		}
		if _, ok := visible.statics[fqn]; ok {
			return true
		}
	}
	if ns == "" {
		return false
	}
	if _, ok := r.csharpCallerEnclosing(e, visible)[ns]; ok {
		return true
	}
	_, ok := visible.imported[ns]
	return ok
}

// csharpExtensionGuardKeep is the cross-package guard's keep-rule for
// extension binds: visibility, not imports, is what makes an extension
// callable, so a bind whose declaring namespace the calling file can see
// must survive the import-reachability revert.
func (r *Resolver) csharpExtensionGuardKeep(e *graph.Edge, callerFile string, target *graph.Node) bool {
	if e == nil || e.Meta == nil || callerFile == "" {
		return false
	}
	if res, _ := e.Meta["resolution"].(string); res != "extension_method" {
		return false
	}
	if !isCSharpExtension(target) {
		return false
	}
	return r.csharpExtensionVisible(e, callerFile, target)
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
