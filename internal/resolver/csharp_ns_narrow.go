package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// C# namespace narrowing. A using directive imports a whole namespace,
// not a name — unlike PHP there is no per-reference FQN to stamp at
// extraction. The evidence is already in the graph instead: using
// directives are EdgeImports off the file node, and every C# type
// carries Meta["scope_ns"]. Joining the two applies the compiler's own
// lookup rule where the bare-name ranking would otherwise tie-break
// same-named types by lexicographic ID — a sibling module's type.

// csharpFileNS is a C# file's visible-namespace evidence, split the way
// the compiler consults it: enclosing namespaces are searched before
// using directives, so an own-namespace type shadows an imported one.
type csharpFileNS struct {
	enclosing map[string]struct{} // declared namespaces + their prefixes
	imported  map[string]struct{} // using-directive namespaces
}

// csharpFileNamespaceSet returns the namespaces visible to a C# file:
// its using-directive imports (pre-resolution unresolved::import:: or
// post-resolution external:: shape — in-pass ordering must not matter)
// plus the file's own declared namespaces and their enclosing prefixes.
func (r *Resolver) csharpFileNamespaceSet(fileID string) csharpFileNS {
	r.csharpNSMu.RLock()
	ns, ok := r.csharpNSByFile[fileID]
	r.csharpNSMu.RUnlock()
	if ok {
		return ns
	}

	ns = csharpFileNS{enclosing: map[string]struct{}{}, imported: map[string]struct{}{}}
	for _, e := range r.graph.GetOutEdges(fileID) {
		if e == nil {
			continue
		}
		switch e.Kind {
		case graph.EdgeImports:
			imp := e.To
			switch {
			case strings.HasPrefix(imp, "unresolved::import::"):
				imp = strings.TrimPrefix(imp, "unresolved::import::")
			case strings.HasPrefix(imp, "external::"):
				imp = strings.TrimPrefix(imp, "external::")
			default:
				continue
			}
			if imp != "" {
				ns.imported[strings.ReplaceAll(imp, "/", ".")] = struct{}{}
			}
		case graph.EdgeDefines:
			n := r.cachedGetNode(e.To)
			if n == nil {
				continue
			}
			scope, _ := n.Meta["scope_ns"].(string)
			for scope != "" {
				ns.enclosing[scope] = struct{}{}
				i := strings.LastIndex(scope, ".")
				if i < 0 {
					break
				}
				scope = scope[:i]
			}
		}
	}

	r.csharpNSMu.Lock()
	if r.csharpNSByFile == nil {
		r.csharpNSByFile = map[string]csharpFileNS{}
	}
	r.csharpNSByFile[fileID] = ns
	r.csharpNSMu.Unlock()
	return ns
}

// csharpNarrowByNamespace filters same-named C# type candidates to the
// ones declared in a namespace the referencing file can see, enclosing
// namespaces first (deepest match wins, mirroring inner-to-outer scope
// search) and using directives second. Same contract as
// phpNarrowByTargetFQN: narrowing only — nil when no candidate
// namespace is visible — so a binding can sharpen but never be lost.
func (r *Resolver) csharpNarrowByNamespace(e *graph.Edge, candidates []*graph.Node) []*graph.Node {
	if len(candidates) < 2 || !strings.HasSuffix(e.FilePath, ".cs") {
		return nil
	}

	// A qualifier written at the reference site (Meta["target_fqn"], from
	// a qualified spelling like Shared.Reporting.Foo) is the strongest
	// evidence: keep only candidates whose namespace ends in it. C#
	// resolves partial qualifiers against visible namespaces, so a suffix
	// match on a dot boundary covers both the full and partial forms.
	qualified := candidates
	if fqn, _ := e.Meta["target_fqn"].(string); strings.Contains(fqn, ".") {
		q := fqn[:strings.LastIndex(fqn, ".")]
		var m []*graph.Node
		for _, c := range candidates {
			if c == nil || c.Language != "csharp" {
				continue
			}
			ns, _ := c.Meta["scope_ns"].(string)
			if ns == q || strings.HasSuffix(ns, "."+q) {
				m = append(m, c)
			}
		}
		if len(m) == 1 {
			return m
		}
		if len(m) > 1 {
			qualified = m
		}
	}

	visible := r.csharpFileNamespaceSet(e.FilePath)
	if len(visible.enclosing) == 0 && len(visible.imported) == 0 {
		if len(qualified) < len(candidates) {
			return qualified
		}
		return nil
	}
	var enclosing, imported []*graph.Node
	deepest := 0
	for _, c := range qualified {
		if c == nil || c.Language != "csharp" {
			continue
		}
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
	if len(imported) > 0 {
		return imported
	}
	// The qualifier narrowed but no visible-namespace tier confirmed —
	// the written qualifier alone still beats a bare-name guess.
	if len(qualified) < len(candidates) {
		return qualified
	}
	return nil
}
