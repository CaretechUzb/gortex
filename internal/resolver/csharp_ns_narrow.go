package resolver

import (
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// C# namespace narrowing. A using directive imports a whole namespace,
// not a name, so there is no per-reference FQN to stamp at extraction
// (the PHP approach). Instead, join what the graph already holds — the
// file's EdgeImports and each type's Meta["scope_ns"] — so a bare-name
// tie no longer falls to lexicographic ID (a sibling module's type).

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
	// Primary using evidence is the extractor's Meta["usings"] stamp —
	// resolveImport rewrites the per-directive edges (a namespace tail
	// matching any directory basename becomes a file-node target), so
	// only the stamp is order-independent. The edge shapes below remain
	// as a fallback for graphs extracted before the stamp existed.
	if f := r.cachedGetNode(fileID); f != nil {
		switch v := f.Meta["usings"].(type) {
		case []string:
			for _, u := range v {
				ns.imported[u] = struct{}{}
			}
		case []any:
			for _, u := range v {
				if s, ok := u.(string); ok {
					ns.imported[s] = struct{}{}
				}
			}
		}
	}
	hasStamp := len(ns.imported) > 0
	for _, e := range r.graph.GetOutEdges(fileID) {
		if e == nil {
			continue
		}
		switch e.Kind {
		case graph.EdgeImports:
			if hasStamp {
				continue
			}
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

// csharpNarrowEligible gates narrowing to type-shaped candidates in the
// dotnet family. Methods carry scope_ns too — letting one match would
// steal the bind from (or lose it for) the type the reference names.
func csharpNarrowEligible(c *graph.Node) bool {
	return c != nil && (c.Kind == graph.KindType || c.Kind == graph.KindInterface) &&
		sameLanguageFamily("csharp", c.Language)
}

// csharpNarrowByNamespace filters same-named C# type candidates to the
// namespaces the referencing file can see: written qualifier, then
// enclosing namespaces (deepest wins), then using directives. Same
// contract as phpNarrowByTargetFQN — narrowing only, never a loss.
func (r *Resolver) csharpNarrowByNamespace(e *graph.Edge, candidates []*graph.Node) []*graph.Node {
	if len(candidates) < 2 || !strings.HasSuffix(e.FilePath, ".cs") {
		return nil
	}

	// A written qualifier (Meta["target_fqn"]) is the strongest evidence.
	// C# resolves partial qualifiers against visible namespaces, so a
	// dot-boundary suffix match covers the full and partial forms alike.
	qualified := candidates
	if fqn, _ := e.Meta["target_fqn"].(string); strings.Contains(fqn, ".") {
		q := fqn[:strings.LastIndex(fqn, ".")]
		dotQ := "." + q
		var m []*graph.Node
		for _, c := range candidates {
			if !csharpNarrowEligible(c) {
				continue
			}
			ns, _ := c.Meta["scope_ns"].(string)
			if ns == q || strings.HasSuffix(ns, dotQ) {
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
		if !csharpNarrowEligible(c) {
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
