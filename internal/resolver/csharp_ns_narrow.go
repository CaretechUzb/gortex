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

// csharpFileNamespaceSet returns the namespaces visible to a C# file:
// its using-directive imports (pre-resolution unresolved::import:: or
// post-resolution external:: shape — in-pass ordering must not matter)
// plus the file's own declared namespaces and their enclosing prefixes.
func (r *Resolver) csharpFileNamespaceSet(fileID string) map[string]struct{} {
	r.csharpNSMu.RLock()
	set, ok := r.csharpNSByFile[fileID]
	r.csharpNSMu.RUnlock()
	if ok {
		return set
	}

	set = map[string]struct{}{}
	for _, e := range r.graph.GetOutEdges(fileID) {
		if e == nil {
			continue
		}
		switch e.Kind {
		case graph.EdgeImports:
			ns := e.To
			switch {
			case strings.HasPrefix(ns, "unresolved::import::"):
				ns = strings.TrimPrefix(ns, "unresolved::import::")
			case strings.HasPrefix(ns, "external::"):
				ns = strings.TrimPrefix(ns, "external::")
			default:
				continue
			}
			if ns != "" {
				set[strings.ReplaceAll(ns, "/", ".")] = struct{}{}
			}
		case graph.EdgeDefines:
			n := r.cachedGetNode(e.To)
			if n == nil {
				continue
			}
			ns, _ := n.Meta["scope_ns"].(string)
			for ns != "" {
				set[ns] = struct{}{}
				i := strings.LastIndex(ns, ".")
				if i < 0 {
					break
				}
				ns = ns[:i]
			}
		}
	}

	r.csharpNSMu.Lock()
	if r.csharpNSByFile == nil {
		r.csharpNSByFile = map[string]map[string]struct{}{}
	}
	r.csharpNSByFile[fileID] = set
	r.csharpNSMu.Unlock()
	return set
}

// csharpNarrowByNamespace filters same-named C# type candidates to the
// ones declared in a namespace the referencing file can see. Same
// contract as phpNarrowByTargetFQN: narrowing only — nil when no
// candidate namespace is visible — so a binding can sharpen but never
// be lost.
func (r *Resolver) csharpNarrowByNamespace(e *graph.Edge, candidates []*graph.Node) []*graph.Node {
	if len(candidates) < 2 || !strings.HasSuffix(e.FilePath, ".cs") {
		return nil
	}
	visible := r.csharpFileNamespaceSet(e.FilePath)
	if len(visible) == 0 {
		return nil
	}
	var out []*graph.Node
	for _, c := range candidates {
		if c == nil || c.Language != "csharp" {
			continue
		}
		ns, _ := c.Meta["scope_ns"].(string)
		if ns == "" {
			continue
		}
		if _, ok := visible[ns]; ok {
			out = append(out, c)
		}
	}
	return out
}
