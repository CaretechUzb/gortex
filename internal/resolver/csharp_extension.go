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
//   - with receiver-type evidence matching NO candidate: refuse — the
//     evidence contradicts them all, and visibility is not applicability.
//   - without receiver-type evidence: bind when namespace visibility narrows
//     the same-name extensions to exactly one, else when the name maps to
//     exactly one extension method in the repo; otherwise stay unresolved.
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
		repo := r.callerRepoPrefix(e)
		recvTrim := csharpTypeSuffixTrim(receiverType)
		recvKey := csharpExtTypeKey(receiverType)
		recvNS := csharpNSPrefix(recvTrim)
		var typed, universal []*graph.Node
		for _, c := range exts {
			tp, _ := c.Meta["this_param_type"].(string)
			if tp == "" {
				continue
			}
			// `this T` (the method's own type parameter) and
			// `this object` match any receiver — they are exempt from
			// the contradiction veto but earn no typed-tier confidence.
			if g, _ := c.Meta["this_param_generic"].(bool); g || csharpTypeSuffixTrim(tp) == "object" {
				universal = append(universal, c)
				continue
			}
			tpTrim := csharpTypeSuffixTrim(tp)
			if tpTrim == recvTrim {
				typed = append(typed, c)
				continue
			}
			// Last-segment match (qualified receiver vs the bare,
			// namespace-stripped this-param): the bare name must DENOTE
			// the receiver's type from the extension's own file — the
			// innermost enclosing namespace that declares a same-name
			// type claims the name (C# shadowing), and only failing
			// that may a visible namespace supply it. Otherwise
			// `Data.Inner` would falsely match an unrelated
			// `Vendor.Inner` extension.
			if csharpExtTypeKey(tp) == recvKey && recvNS != "" &&
				r.csharpParamDenotes(c, tpTrim, recvTrim, repo) {
				typed = append(typed, c)
			}
		}
		if len(typed) == 0 {
			// No name-level match: the receiver may still REACH a
			// candidate's this-param through its base/interface chain
			// (`Crate : IBox` calling `Foo(this IBox)`).
			reached, waive := r.csharpExtensionReachableMatches(e, recvTrim, exts)
			typed = reached
			if len(typed) == 0 {
				// The any-receiver candidates stay eligible; an
				// incomplete hierarchy (an unresolved external base)
				// additionally waives the veto for candidates that
				// unknown part could satisfy — a this-param that is
				// neither in-repo (an external base can never derive
				// from a repo type) nor a sealed builtin. Everything
				// else stays provably unrelated: visibility is not
				// applicability, and binding would let a visible
				// same-name extension swallow e.g. a BCL instance call
				// (`xs.Add(1)`). A missing edge, never a wrong one.
				pool := append([]*graph.Node(nil), universal...)
				if waive {
					pool = append(pool, r.csharpWaiverEligible(exts, universal, repo)...)
				}
				if len(pool) > 0 {
					return r.bindCSharpExtensionUntyped(e, pool, candidates, stats)
				}
				return false
			}
		}
		if len(typed) == 1 {
			if !r.csharpExtensionVisible(e, e.FilePath, typed[0]) {
				// An extension the call site can neither enclose nor
				// import is uncallable C# — refusing upfront is the
				// verdict the guard keep-rule would reach anyway, minus
				// the bind/revert churn on every pass.
				return false
			}
			r.bindCSharpExtension(e, typed[0], 0.9, stats)
			return true
		}
		// Same receiver type in several namespaces (the per-module
		// DI-configuration pattern) — the call site's visible
		// namespaces break the tie the type alone cannot.
		if vis := r.narrowCSharpExtensionsByVisibility(e, typed); len(vis) == 1 {
			r.bindCSharpExtension(e, vis[0], 0.9, stats)
			return true
		}
		return false
	}

	return r.bindCSharpExtensionUntyped(e, exts, candidates, stats)
}

// csharpWaiverEligible filters the extension pool to the candidates an
// UNKNOWN part of the receiver's hierarchy could satisfy: a this-param
// type that is absent from the repo and not a sealed builtin (plus
// candidates with no param evidence at all). In-repo and builtin
// this-params remain provably unrelated even under an incomplete
// hierarchy. Universal candidates are the caller's to add.
func (r *Resolver) csharpWaiverEligible(exts, universal []*graph.Node, repo string) []*graph.Node {
	isUniversal := map[string]bool{}
	for _, u := range universal {
		isUniversal[u.ID] = true
	}
	var out []*graph.Node
	for _, c := range exts {
		if isUniversal[c.ID] {
			continue
		}
		tp, _ := c.Meta["this_param_type"].(string)
		if tp == "" {
			out = append(out, c)
			continue
		}
		tpTrim := csharpTypeSuffixTrim(tp)
		if csharpIsBuiltinTypeName(tpTrim) {
			continue
		}
		if len(r.csharpTypeNodesNamed(csharpExtTypeKey(tpTrim), repo)) > 0 {
			continue
		}
		out = append(out, c)
	}
	return out
}

// bindCSharpExtensionUntyped applies the no-type-evidence rules to a
// candidate pool: visibility narrowing first, then the pool-unique
// name. Either way a non-extension member of the same name blocks the
// bind (C# instance-method precedence — let the locality fallback bind
// the instance method instead).
func (r *Resolver) bindCSharpExtensionUntyped(e *graph.Edge, pool []*graph.Node, candidates []*graph.Node, stats *ResolveStats) bool {
	if len(pool) == 0 {
		return false
	}
	if vis := r.narrowCSharpExtensionsByVisibility(e, pool); len(vis) == 1 {
		pool = vis
	}
	if len(pool) == 1 && !csharpHasCompetingMethod(candidates) {
		r.bindCSharpExtension(e, pool[0], 0.75, stats)
		return true
	}
	return false
}

// csharpExtensionReachableMatches walks the receiver type's declared
// base/interface chain upward and returns the (deduped) extensions
// whose this-param it reaches. waive=true when the receiver is in-repo
// but its hierarchy carries an unresolved edge (an external base) — an
// "unrelated" verdict is unreliable there. Both empty: the receiver is
// either absent from the repo, not denotable at the call site, or
// provably unrelated to every candidate.
//
// Both ends are anchored: the walk starts only from the type the
// receiver can DENOTE at the call site (not every same-name type in the
// repo — an unrelated namespace's hierarchy must not donate a match),
// and a reached type satisfies a candidate only when the candidate's
// bare this-param denotes it from the extension's own file (the same
// shadowing rule the direct match uses).
func (r *Resolver) csharpExtensionReachableMatches(e *graph.Edge, recv string, exts []*graph.Node) (matches []*graph.Node, waive bool) {
	type wantEntry struct {
		c      *graph.Node
		tpTrim string
	}
	want := map[string][]wantEntry{}
	for _, c := range exts {
		tp, _ := c.Meta["this_param_type"].(string)
		if g, _ := c.Meta["this_param_generic"].(bool); g || tp == "" {
			continue
		}
		tpTrim := csharpTypeSuffixTrim(tp)
		want[csharpExtTypeKey(tpTrim)] = append(want[csharpExtTypeKey(tpTrim)], wantEntry{c: c, tpTrim: tpTrim})
	}
	if len(want) == 0 {
		// Universal-only pools have nothing to reach — skip the walk.
		return nil, false
	}
	repo := r.callerRepoPrefix(e)
	start := r.csharpReceiverTypeNodes(e, recv, repo)
	if len(start) == 0 {
		return nil, false
	}
	incomplete := false
	ancestorIDs := map[string]struct{}{}
	for _, s := range start {
		anc := r.csharpAncestorsOf(s, repo)
		incomplete = incomplete || anc.incomplete
		for id := range anc.ids {
			ancestorIDs[id] = struct{}{}
		}
	}
	seenMatch := map[string]bool{}
	for id := range ancestorIDs {
		p := r.cachedGetNode(id)
		if p == nil {
			continue
		}
		for _, w := range want[p.Name] {
			if !seenMatch[w.c.ID] && r.csharpParamDenotes(w.c, w.tpTrim, csharpNodeFQN(p), repo) {
				seenMatch[w.c.ID] = true
				matches = append(matches, w.c)
			}
		}
	}
	return matches, incomplete && len(matches) == 0
}

// csharpReceiverTypeNodes resolves what the receiver evidence can
// denote at the call site. A qualified receiver names its namespace
// outright (exact or partially-qualified suffix). A bare receiver
// follows C# lookup: the calling method's own enclosing-namespace chain
// claims the name first (deepest wins), then the global namespace, then
// the file's using directives; a same-name type in a namespace the call
// site cannot see denotes nothing — its hierarchy must not stand in for
// the receiver's.
func (r *Resolver) csharpReceiverTypeNodes(e *graph.Edge, recv, repo string) []*graph.Node {
	if recvNS := csharpNSPrefix(recv); recvNS != "" {
		var out []*graph.Node
		for _, n := range r.csharpTypeNodesNamed(csharpExtTypeKey(recv), repo) {
			if fqn := csharpNodeFQN(n); fqn == recv || strings.HasSuffix(fqn, "."+recv) {
				out = append(out, n)
			}
		}
		return out
	}
	scope := ""
	if cn := r.cachedGetNode(e.From); cn != nil {
		scope, _ = cn.Meta["scope_ns"].(string)
	}
	return r.csharpDenoteBareType(recv, scope, e.FilePath, repo)
}

// csharpDenoteBareType resolves what a bare type name denotes from a
// position in the code: the enclosing-namespace chain claims the name
// first (deepest wins; the file's declared-namespace union when the
// position carries no scope stamp — older graphs), then the global
// namespace, then the file's using directives. Nil when the name
// denotes nothing in-repo from there.
func (r *Resolver) csharpDenoteBareType(name, scope, fileID, repo string) []*graph.Node {
	pool := r.csharpTypeNodesNamed(name, repo)
	if len(pool) == 0 {
		return nil
	}
	byNS := func(ns string) []*graph.Node {
		var out []*graph.Node
		for _, n := range pool {
			if s, _ := n.Meta["scope_ns"].(string); s == ns {
				out = append(out, n)
			}
		}
		return out
	}
	visible := r.csharpFileNamespaceSet(fileID)
	if scope != "" {
		for {
			if out := byNS(scope); len(out) > 0 {
				return out
			}
			i := strings.LastIndex(scope, ".")
			if i < 0 {
				break
			}
			scope = scope[:i]
		}
	} else {
		var out []*graph.Node
		for _, n := range pool {
			s, _ := n.Meta["scope_ns"].(string)
			if _, ok := visible.enclosing[s]; ok {
				out = append(out, n)
			}
		}
		if len(out) > 0 {
			return out
		}
	}
	if out := byNS(""); len(out) > 0 {
		return out
	}
	var out []*graph.Node
	for _, n := range pool {
		s, _ := n.Meta["scope_ns"].(string)
		if s == "" {
			continue
		}
		if _, ok := visible.imported[s]; ok {
			out = append(out, n)
		}
	}
	return out
}

// csharpParamDenotes reports whether an extension's this-param type
// name can denote the target type. A qualified param names its
// namespace explicitly. A bare param resolves from the extension's own
// position: the innermost enclosing namespace that declares a
// same-name type claims the name (C# shadowing) — the target must BE
// that type; only when no enclosing level claims it may any namespace
// visible from the extension's file supply the target.
func (r *Resolver) csharpParamDenotes(c *graph.Node, tpTrim, targetFQN, repo string) bool {
	fqnMatches := func(fqn, want string) bool {
		return fqn == want || strings.HasSuffix(fqn, "."+want)
	}
	if csharpNSPrefix(tpTrim) != "" {
		return fqnMatches(targetFQN, tpTrim)
	}
	pool := r.csharpTypeNodesNamed(tpTrim, repo)
	scope, _ := c.Meta["scope_ns"].(string)
	for scope != "" {
		claimed := false
		for _, n := range pool {
			ns, _ := n.Meta["scope_ns"].(string)
			if ns != scope {
				continue
			}
			claimed = true
			if fqnMatches(csharpNodeFQN(n), targetFQN) {
				return true
			}
		}
		if claimed {
			return false
		}
		i := strings.LastIndex(scope, ".")
		if i < 0 {
			break
		}
		scope = scope[:i]
	}
	// No enclosing claim — a visible namespace may supply the target.
	tns := csharpNSPrefix(targetFQN)
	if tns == "" {
		return true
	}
	return r.csharpNamespaceVisibleFrom(c.FilePath, tns)
}

// csharpAncestors is one type node's transitive base/interface closure.
type csharpAncestors struct {
	ids        map[string]struct{}
	incomplete bool
}

// csharpAncestorsOf returns the memoized transitive Extends/Implements
// closure of a type node — the walk is caller-independent, so a popular
// receiver type costs one BFS per pass instead of one per call site.
func (r *Resolver) csharpAncestorsOf(n *graph.Node, repo string) *csharpAncestors {
	r.csharpNSMu.RLock()
	cached := r.csharpAncestorsByType[n.ID]
	r.csharpNSMu.RUnlock()
	if cached != nil {
		return cached
	}
	anc := &csharpAncestors{ids: map[string]struct{}{}}
	seen := map[string]bool{n.ID: true}
	queue := []string{n.ID}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		r.csharpNSMu.RLock()
		memo := r.csharpAncestorsByType[id]
		r.csharpNSMu.RUnlock()
		if id != n.ID && memo != nil {
			for a := range memo.ids {
				anc.ids[a] = struct{}{}
			}
			anc.incomplete = anc.incomplete || memo.incomplete
			continue
		}
		for _, ed := range r.graph.GetOutEdges(id) {
			if ed == nil || (ed.Kind != graph.EdgeExtends && ed.Kind != graph.EdgeImplements) {
				continue
			}
			if graph.IsUnresolvedTarget(ed.To) {
				// The base may simply not have resolved yet this pass
				// (in-page edge ordering) — chase the stub by name from
				// the child's own position, the same anchored lookup the
				// resolver will apply to the edge itself. Only a name
				// that denotes nothing in-repo means a genuinely
				// external base.
				chased := r.csharpStubBaseNodes(id, ed.To, repo)
				if len(chased) == 0 {
					anc.incomplete = true
					continue
				}
				for _, p := range chased {
					if !seen[p.ID] {
						seen[p.ID] = true
						anc.ids[p.ID] = struct{}{}
						queue = append(queue, p.ID)
					}
				}
				continue
			}
			if ed.To == "" || seen[ed.To] {
				continue
			}
			seen[ed.To] = true
			anc.ids[ed.To] = struct{}{}
			queue = append(queue, ed.To)
		}
	}
	r.csharpNSMu.Lock()
	if r.csharpAncestorsByType == nil {
		r.csharpAncestorsByType = map[string]*csharpAncestors{}
	}
	r.csharpAncestorsByType[n.ID] = anc
	r.csharpNSMu.Unlock()
	return anc
}

// csharpStubBaseNodes resolves an unresolved base-list stub the way the
// resolver eventually will: a qualified name matches on its namespace,
// a bare one denotes from the child type's own position.
func (r *Resolver) csharpStubBaseNodes(childID, stub, repo string) []*graph.Node {
	name := csharpTypeSuffixTrim(graph.UnresolvedName(stub))
	if name == "" || strings.HasPrefix(name, "*.") {
		return nil
	}
	if ns := csharpNSPrefix(name); ns != "" {
		var out []*graph.Node
		for _, n := range r.csharpTypeNodesNamed(csharpExtTypeKey(name), repo) {
			if fqn := csharpNodeFQN(n); fqn == name || strings.HasSuffix(fqn, "."+name) {
				out = append(out, n)
			}
		}
		return out
	}
	child := r.cachedGetNode(childID)
	if child == nil {
		return nil
	}
	scope, _ := child.Meta["scope_ns"].(string)
	return r.csharpDenoteBareType(name, scope, child.FilePath, repo)
}

// csharpTypeNodesNamed is the memoized repo-scoped C# type/interface
// lookup for a bare type name — the eligibility rules consult it once
// per candidate per edge, and the underlying store query repeated per
// edge is exactly the storm the name-cache comment warns about.
func (r *Resolver) csharpTypeNodesNamed(name, repo string) []*graph.Node {
	key := repo + "\x00" + name
	r.csharpNSMu.RLock()
	cached, ok := r.csharpTypeNodesByName[key]
	r.csharpNSMu.RUnlock()
	if ok {
		return cached
	}
	var out []*graph.Node
	for _, n := range r.cachedFindNodesByNameInRepo(name, repo) {
		if n != nil && (n.Kind == graph.KindType || n.Kind == graph.KindInterface) &&
			sameLanguageFamily("csharp", n.Language) {
			out = append(out, n)
		}
	}
	r.csharpNSMu.Lock()
	if r.csharpTypeNodesByName == nil {
		r.csharpTypeNodesByName = map[string][]*graph.Node{}
	}
	r.csharpTypeNodesByName[key] = out
	r.csharpNSMu.Unlock()
	return out
}

// csharpNodeFQN is a type node's namespace-qualified name.
func csharpNodeFQN(n *graph.Node) string {
	if ns, _ := n.Meta["scope_ns"].(string); ns != "" {
		return ns + "." + n.Name
	}
	return n.Name
}

// csharpIsBuiltinTypeName reports whether a (suffix-trimmed) type name
// is a sealed C# builtin — a type no hierarchy can ever reach.
func csharpIsBuiltinTypeName(t string) bool {
	switch t {
	case "string", "int", "long", "short", "byte", "sbyte", "uint",
		"ulong", "ushort", "float", "double", "decimal", "bool", "char":
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
		if csharpUsingStaticAdmits(visible, c, ns) {
			imported = append(imported, c)
			continue
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

// csharpTypeSuffixTrim canonicalises a type name's suffixes — nullable,
// array, generics — and folds the BCL alias forms (`String`,
// `System.Int32`) onto their keyword spellings, leaving other namespace
// qualification intact. The two comparison sides arrive differently
// normalised (this_param_type keeps ?/[] and strips namespace; the
// call-site receiver_type does the opposite), so the safe verbatim
// compare runs on these forms.
func csharpTypeSuffixTrim(t string) string {
	t = strings.TrimSpace(t)
	// Strip to fixpoint — `int?[]` and `int[]?` spell the same evidence.
	for {
		trimmed := strings.TrimSuffix(t, "?")
		if i := strings.Index(trimmed, "["); i > 0 {
			trimmed = trimmed[:i]
		}
		if i := strings.Index(trimmed, "<"); i > 0 {
			trimmed = trimmed[:i]
		}
		if trimmed == t {
			break
		}
		t = trimmed
	}
	t = strings.TrimPrefix(t, "System.")
	switch t {
	case "String":
		return "string"
	case "Int32":
		return "int"
	case "Int64":
		return "long"
	case "Int16":
		return "short"
	case "Byte":
		return "byte"
	case "SByte":
		return "sbyte"
	case "UInt32":
		return "uint"
	case "UInt64":
		return "ulong"
	case "UInt16":
		return "ushort"
	case "Single":
		return "float"
	case "Double":
		return "double"
	case "Decimal":
		return "decimal"
	case "Boolean":
		return "bool"
	case "Char":
		return "char"
	case "Object":
		return "object"
	}
	return t
}

// csharpExtTypeKey is csharpTypeSuffixTrim plus last-namespace-segment —
// the loosest comparable core. A key match alone is NOT eligibility:
// the caller must anchor it (see the visibility check at the match
// site), or `Data.Inner` matches an unrelated `Vendor.Inner`.
func csharpExtTypeKey(t string) string {
	t = csharpTypeSuffixTrim(t)
	if i := strings.LastIndex(t, "."); i >= 0 {
		t = t[i+1:]
	}
	return t
}

// csharpNSPrefix returns the namespace qualifier of a (suffix-trimmed)
// type name — "" when unqualified.
func csharpNSPrefix(t string) string {
	if i := strings.LastIndex(t, "."); i > 0 {
		return t[:i]
	}
	return ""
}

// csharpUsingStaticAdmits reports whether the candidate extension's
// declaring class (scope_ns + Meta["receiver"]) is a using-static
// target of the visible set.
func csharpUsingStaticAdmits(visible csharpFileNS, c *graph.Node, ns string) bool {
	if len(visible.statics) == 0 {
		return false
	}
	cls, _ := c.Meta["receiver"].(string)
	if cls == "" {
		return false
	}
	fqn := cls
	if ns != "" {
		fqn = ns + "." + cls
	}
	_, ok := visible.statics[fqn]
	return ok
}

// csharpNamespaceVisibleFrom reports whether namespace ns is visible
// from fileID (enclosing or imported) — the anchor a bare this-param
// name needs before it can mean a type from that namespace.
func (r *Resolver) csharpNamespaceVisibleFrom(fileID, ns string) bool {
	visible := r.csharpFileNamespaceSet(fileID)
	if _, ok := visible.enclosing[ns]; ok {
		return true
	}
	_, ok := visible.imported[ns]
	return ok
}

// csharpExtensionVisible reports whether an extension method's declaring
// namespace is visible from the call site — via the caller's enclosing-
// namespace chain, a using directive (project-scoped globals included),
// or a using-static of the declaring class. Same evidence the narrowing
// uses, so the guard keep-rule and the bind can never disagree.
func (r *Resolver) csharpExtensionVisible(e *graph.Edge, fileID string, c *graph.Node) bool {
	visible := r.csharpFileNamespaceSet(fileID)
	ns, _ := c.Meta["scope_ns"].(string)
	if csharpUsingStaticAdmits(visible, c, ns) {
		return true
	}
	if ns == "" {
		// A global-namespace extension is in scope everywhere.
		return true
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
