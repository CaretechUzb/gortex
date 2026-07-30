package resolver

import (
	"strconv"

	"github.com/zzet/gortex/internal/graph"
)

// Member-level C# interface-dispatch synthesis: the implements-family cascade.
//
// Roslyn — the reference C# resolver — treats an interface method and every
// method that implements it (directly, or through a base class that implements
// the interface) as ONE linked family, and reports the union of the family's
// call sites for every member. Two mechanisms feed that union:
//
//  1. Through-interface calls: `x.Convert(1)` where `x` is typed as the
//     interface binds to the interface member node. Those calls must surface
//     on every concrete implementation.
//  2. Sibling implementation calls: a converter's own `Convert(-number)`
//     (a self/recursive or same-class call) binds directly to that class's
//     method node — it never touches the interface node. Roslyn still reports
//     that site for the interface method AND for every sibling implementation.
//
// A fan-out anchored only on calls bound to the interface member (mechanism 1)
// misses the dominant mass of real-corpus usages, which are mechanism 2. This
// pass therefore builds the full implements-family per (interface, method
// name) — the interface member plus the same-named method on every type whose
// implements/extends chain reaches the interface — and, for every call edge
// bound to ANY family member, synthesizes call edges to ALL other members.
//
// Tier: ast_inferred / ConfidenceTyped (non-speculative, type-keyed) — the
// same tier the sibling one-to-many dispatch passes use (MediatR Publish ->
// every handler, Spring publishEvent -> every listener), so the cascade rides
// in the DEFAULT find_usages / get_callers result. Family membership is
// established strictly through the implements/extends chain — never by name
// matching alone — so unrelated same-named methods are never linked.

// csharpIfaceDispatchCap bounds the family size (every interface-member
// overload node plus every implementing method node). C# overloads mint one
// node each, so a broadly-localised interface — one implementation per locale,
// several overloads per class — legitimately runs to ~70+ member nodes
// (Humanizer's INumberToWordsConverter.Convert family measures 72) and is
// exactly the shape this pass exists to cover, so the cap sits above it with
// headroom; a family wider than the cap is dropped whole as noise
// (pathological hub interfaces like a monorepo-wide Dispose).
const csharpIfaceDispatchCap = 128

// MetaViaMethodSetInference is the Meta["via"] marker the resolver stamps on
// EdgeImplements edges minted by structural method-set inference (as opposed
// to a source-declared base list). Hierarchy-walking passes that must follow
// only declared subtyping filter on it.
const MetaViaMethodSetInference = "method-set-inference"

// csharpCallSiteKey identifies one attributed call site. Line is part of the
// key on purpose: ground truth is line-based, so every call-site line of every
// family member must fan out to every other member, not one edge per
// (caller, callee) pair.
func csharpCallSiteKey(from, to, filePath string, line int) string {
	return from + "\x00" + to + "\x00" + filePath + "\x00" + strconv.Itoa(line)
}

// ResolveCSharpInterfaceDispatch fans every call bound to a member of a C#
// implements-family out to all other members of that family. Returns the
// number of fan-out edges landed.
func ResolveCSharpInterfaceDispatch(g graph.Store) int {
	return ResolveCSharpInterfaceDispatchScoped(g, nil)
}

// ResolveCSharpInterfaceDispatchScoped limits partial work to changed
// repositories plus the in-repo interface families targeted by their calls.
// Incoming calls to those exact family members form the reverse dependency
// frontier. A nil scope preserves the full/cold whole-graph behavior.
func ResolveCSharpInterfaceDispatchScoped(g graph.Store, scope map[string]bool) int {
	if g == nil {
		return 0
	}
	familyScope := scope
	scopedSourceCalls := []*graph.Edge(nil)
	if scope != nil {
		familyScope = make(map[string]bool, len(scope))
		for prefix, enabled := range scope {
			if enabled {
				familyScope[prefix] = true
			}
		}
		scopedSourceCalls = frameworkRepoEdges(g, scope, graph.EdgeCalls)
		targetIDs := make([]string, 0, len(scopedSourceCalls))
		for _, edge := range scopedSourceCalls {
			if edge != nil && !graph.IsUnresolvedTarget(edge.To) {
				targetIDs = append(targetIDs, edge.To)
			}
		}
		for _, target := range g.GetNodesByIDs(targetIDs) {
			if target != nil && target.Language == "csharp" && target.Kind == graph.KindMethod {
				familyScope[target.RepoPrefix] = true
			}
		}
	}

	// Subtype adjacency over the resolved type hierarchy: super → subs.
	// EdgeImplements and EdgeExtends both count — a class reaches an interface
	// through any chain of base classes / base interfaces (e.g. Afrikaans
	// extends Genderless which implements INumberToWordsConverter).
	//
	// Only SOURCE-DECLARED hierarchy edges qualify. The method-set inference
	// pass mints EdgeImplements from every type whose bare method names cover
	// an interface — with a single-method interface like IOrdinalizer.Convert
	// that "links" every Convert-bearing class in the repo, and a family built
	// over it would union unrelated hierarchies (NumberToWords converters into
	// the Ordinalizer family). Those edges carry the inference marker; skip
	// them. Origin cannot discriminate here: it is stamped/backfilled at
	// different pipeline stages, so declared and inferred edges converge.
	// This pass can run BEFORE the resolver has bound base-list targets (the
	// pipeline settles hierarchy targets across several later passes), so an
	// `unresolved::Name` target is resolved here by an exact, same-repo,
	// unique type/interface name lookup — ambiguity means skip, never guess.
	hierarchyEdges := frameworkRepoEdges(g, familyScope, graph.EdgeImplements, graph.EdgeExtends)
	hierarchySourceIDs := make([]string, 0, len(hierarchyEdges))
	hierarchyNames := make([]string, 0)
	seenHierarchyNames := map[string]bool{}
	for _, edge := range hierarchyEdges {
		if edge == nil {
			continue
		}
		hierarchySourceIDs = append(hierarchySourceIDs, edge.From)
		if graph.IsUnresolvedTarget(edge.To) {
			name := graph.UnresolvedName(edge.To)
			if name != "" && !seenHierarchyNames[name] {
				seenHierarchyNames[name] = true
				hierarchyNames = append(hierarchyNames, name)
			}
		}
	}
	hierarchySources := g.GetNodesByIDs(hierarchySourceIDs)
	hierarchyByName := g.FindNodesByNames(hierarchyNames)
	children := map[string][]string{}
	for _, e := range hierarchyEdges {
		if e == nil || e.From == "" || e.To == "" {
			continue
		}
		if e.Meta != nil && e.Meta["via"] == MetaViaMethodSetInference {
			continue
		}
		toID := e.To
		if graph.IsUnresolvedTarget(toID) {
			toID = csharpResolveHierarchyTargetPrefetched(hierarchySources[e.From], toID, hierarchyByName)
			if toID == "" {
				continue
			}
		}
		children[toID] = append(children[toID], e.From)
	}
	if len(children) == 0 {
		return 0
	}

	// implementation/interface type node id → member name → method nodes.
	// Every overload matters: C# overloads mint one node each (Convert,
	// Convert_L39, ...) sharing the same Name, and real call sites bind to any
	// of them — a single-node-per-name projection would silently drop the
	// overload the corpus actually calls through.
	memberEdges := frameworkRepoEdges(g, familyScope, graph.EdgeMemberOf)
	memberNodeIDs := make([]string, 0, len(memberEdges))
	for _, edge := range memberEdges {
		if edge != nil {
			memberNodeIDs = append(memberNodeIDs, edge.From)
		}
	}
	memberNodes := g.GetNodesByIDs(memberNodeIDs)
	var membersByType map[string]map[string][]*graph.Node
	if scope == nil {
		membersByType = csharpMemberMethodsAllByType(g)
	} else {
		membersByType = csharpMemberMethodsAllByTypeFromEdges(memberEdges, memberNodes)
	}
	if len(membersByType) == 0 {
		return 0
	}

	// Anchor discovery: every C# interface member method node, via its
	// EdgeMemberOf owner, grouped by (interface, name) so the interface's own
	// overload nodes land in ONE family rather than seeding duplicates.
	type anchorGroup struct {
		ifaceID    string
		name       string
		repoPrefix string
		nodeIDs    []string
	}
	anchorGroups := map[string]*anchorGroup{}
	var anchorOrder []string
	for _, e := range memberEdges {
		if e == nil || graph.IsUnresolvedTarget(e.To) {
			continue
		}
		m := memberNodes[e.From]
		if m == nil || m.Kind != graph.KindMethod || m.Language != "csharp" || !csharpIsIfaceMember(m) {
			continue
		}
		key := e.To + "\x00" + m.Name
		ag := anchorGroups[key]
		if ag == nil {
			ag = &anchorGroup{ifaceID: e.To, name: m.Name, repoPrefix: m.RepoPrefix}
			anchorGroups[key] = ag
			anchorOrder = append(anchorOrder, key)
		}
		ag.nodeIDs = append(ag.nodeIDs, m.ID)
	}
	if len(anchorGroups) == 0 {
		return 0
	}

	// Descendant closure per interface, computed once and shared across that
	// interface's anchors (one per member name).
	descCache := map[string][]string{}
	descendants := func(ifaceID string) []string {
		if d, ok := descCache[ifaceID]; ok {
			return d
		}
		var out []string
		visited := map[string]bool{ifaceID: true}
		queue := append([]string(nil), children[ifaceID]...)
		for len(queue) > 0 {
			t := queue[0]
			queue = queue[1:]
			if visited[t] {
				continue
			}
			visited[t] = true
			out = append(out, t)
			queue = append(queue, children[t]...)
		}
		descCache[ifaceID] = out
		return out
	}

	// Build families and the member → families index.
	type family struct {
		ifaceID string
		members []string
	}
	var families []family
	famsOfMember := map[string][]int{}
	for _, key := range anchorOrder {
		ag := anchorGroups[key]
		memberIDs := append([]string(nil), ag.nodeIDs...)
		anchorSet := map[string]bool{}
		for _, id := range ag.nodeIDs {
			anchorSet[id] = true
		}
		implCount := 0
		for _, sub := range descendants(ag.ifaceID) {
			byName := membersByType[sub]
			if byName == nil {
				continue
			}
			for _, m := range byName[ag.name] {
				if m == nil || anchorSet[m.ID] {
					continue
				}
				// In-repo only: cross-repo dispatch is CrossRepoResolver's domain.
				if m.RepoPrefix != ag.repoPrefix {
					continue
				}
				memberIDs = append(memberIDs, m.ID)
				implCount++
			}
		}
		// A family needs an interface member plus at least one implementation
		// to cascade; one wider than the cap is dropped whole as noise.
		if implCount == 0 || len(memberIDs) > csharpIfaceDispatchCap {
			continue
		}
		idx := len(families)
		families = append(families, family{ifaceID: ag.ifaceID, members: memberIDs})
		for _, id := range memberIDs {
			famsOfMember[id] = append(famsOfMember[id], idx)
		}
	}
	if len(families) == 0 {
		return 0
	}
	callEdges := frameworkEdgesByKinds(g, graph.EdgeCalls)
	if scope != nil {
		callEdges = nil
		callSeen := make(map[graph.EdgeIdentity]struct{})
		callEdges = appendUniqueFrameworkEdges(callEdges, callSeen, scopedSourceCalls...)
		familyMemberIDs := make([]string, 0, len(famsOfMember))
		for id := range famsOfMember {
			familyMemberIDs = append(familyMemberIDs, id)
		}
		for _, incoming := range g.GetInEdgesByNodeIDs(familyMemberIDs) {
			for _, edge := range incoming {
				if edge != nil && edge.Kind == graph.EdgeCalls {
					callEdges = appendUniqueFrameworkEdges(callEdges, callSeen, edge)
				}
			}
		}
	}

	// Existing resolved call sites, keyed per line, so a fan-out edge never
	// duplicates a real call at the same site (a caller that already reaches
	// the member directly, or a prior run of this pass).
	existing := map[string]bool{}
	for _, e := range callEdges {
		if e == nil || e.IsSpeculative() || graph.IsUnresolvedTarget(e.To) {
			continue
		}
		existing[csharpCallSiteKey(e.From, e.To, e.FilePath, e.Line)] = true
	}

	var batch []*graph.Edge
	seen := map[string]bool{}
	for _, e := range callEdges {
		if e == nil || e.IsSpeculative() || graph.IsUnresolvedTarget(e.To) {
			continue
		}
		// Never re-fan from this pass's own output — real call sites only.
		if e.Meta != nil && e.Meta[MetaSynthesizedBy] == SynthCSharpIfaceDispatch {
			continue
		}
		fams := famsOfMember[e.To]
		if len(fams) == 0 {
			continue
		}
		// Tier-gate the SOURCE: a typed or scope-resolved binding (and an
		// untagged legacy edge, which carries unknown — not low — confidence,
		// mirroring SuppressRedundantTextMatches) fans from any caller. A
		// text_matched binding is a name-only guess that can land on a family
		// member from a completely unrelated same-named method (an
		// IOrdinalizer.Convert self-call text-matched into the
		// INumberToWordsConverter family); those fan ONLY when the caller is
		// itself a member of the same family — the intra-family self/sibling-
		// call shape the weak tier legitimately carries (overload self-calls
		// bind text_matched).
		weakSource := e.Origin == graph.OriginTextMatched
		var fromFams []int
		if weakSource {
			fromFams = famsOfMember[e.From]
			if len(fromFams) == 0 {
				continue
			}
		}
		for _, fi := range fams {
			if weakSource && !containsInt(fromFams, fi) {
				continue
			}
			f := families[fi]
			for _, member := range f.members {
				if member == e.To {
					continue
				}
				k := csharpCallSiteKey(e.From, member, e.FilePath, e.Line)
				if existing[k] || seen[k] {
					continue
				}
				seen[k] = true
				batch = append(batch, csharpIfaceDispatchEdge(e, member, f.ifaceID, len(f.members)-1))
			}
		}
	}
	if len(batch) > 0 {
		g.AddBatch(nil, batch)
	}
	return len(batch)
}

// csharpIsIfaceMember reports whether n is a bodyless (or default) interface
// member declaration emitted by the C# extractor.
func csharpIsIfaceMember(n *graph.Node) bool {
	if n == nil || n.Meta == nil {
		return false
	}
	v, _ := n.Meta["iface_member"].(bool)
	return v
}

// csharpIfaceDispatchEdge builds one fan-out call edge from the call site e to
// another family member, at the non-speculative ast_inferred tier so it
// survives the default speculative filter on find_usages / get_callers. The
// fan-out width rides in candidate_count for auditing; only one implementation
// runs at a site, but Roslyn reports the reference on every family member and
// this pass mirrors that.
func csharpIfaceDispatchEdge(e *graph.Edge, to, ifaceTypeID string, fanout int) *graph.Edge {
	ne := &graph.Edge{
		From: e.From, To: to, Kind: graph.EdgeCalls,
		FilePath: e.FilePath, Line: e.Line,
		Origin:          graph.OriginASTInferred,
		Confidence:      ConfidenceTyped,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, ConfidenceTyped),
		Meta: map[string]any{
			"via":             "csharp-iface-dispatch",
			"iface_type":      ifaceTypeID,
			"candidate_count": fanout,
		},
	}
	StampSynthesized(ne, SynthCSharpIfaceDispatch)
	return ne
}

// csharpMemberMethodsAllByType is the overload-preserving variant of
// memberMethodNodesByType: type node id → member name → EVERY method node with
// that name (C# overloads mint one node per declaration, so a name maps to
// several nodes). Uses the backend's MemberMethodsByType projection when
// available, else walks EdgeMemberOf.
func csharpMemberMethodsAllByType(g graph.Store) map[string]map[string][]*graph.Node {
	if cap, ok := g.(graph.MemberMethodsByType); ok {
		raw := cap.MemberMethodsByType()
		if len(raw) == 0 {
			return nil
		}
		out := make(map[string]map[string][]*graph.Node, len(raw))
		for typeID, methods := range raw {
			set := make(map[string][]*graph.Node, len(methods))
			for _, m := range methods {
				set[m.Name] = append(set[m.Name], &graph.Node{
					ID:         m.MethodID,
					Kind:       graph.KindMethod,
					Name:       m.Name,
					FilePath:   m.FilePath,
					StartLine:  m.StartLine,
					RepoPrefix: m.RepoPrefix,
				})
			}
			out[typeID] = set
		}
		return out
	}
	var edges []*graph.Edge
	methodIDs := make([]string, 0)
	for e := range g.EdgesByKind(graph.EdgeMemberOf) {
		if e == nil {
			continue
		}
		edges = append(edges, e)
		methodIDs = append(methodIDs, e.From)
	}
	return csharpMemberMethodsAllByTypeFromEdges(edges, g.GetNodesByIDs(methodIDs))
}

func csharpMemberMethodsAllByTypeFromEdges(edges []*graph.Edge, nodes map[string]*graph.Node) map[string]map[string][]*graph.Node {
	out := map[string]map[string][]*graph.Node{}
	for _, e := range edges {
		if e == nil {
			continue
		}
		method := nodes[e.From]
		if method == nil || method.Kind != graph.KindMethod {
			continue
		}
		set := out[e.To]
		if set == nil {
			set = make(map[string][]*graph.Node)
			out[e.To] = set
		}
		set[method.Name] = append(set[method.Name], method)
	}
	return out
}

// containsInt reports whether xs contains v. Family lists are tiny (a method
// belongs to one or two families), so a linear scan beats a map.
func containsInt(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func csharpResolveHierarchyTargetPrefetched(from *graph.Node, unresolvedTo string, byName map[string][]*graph.Node) string {
	name := graph.UnresolvedName(unresolvedTo)
	if name == "" {
		return ""
	}
	if from == nil || from.Language != "csharp" {
		return ""
	}
	var cand *graph.Node
	for _, n := range byName[name] {
		if n == nil || (n.Kind != graph.KindType && n.Kind != graph.KindInterface) {
			continue
		}
		if n.Language != "csharp" || n.RepoPrefix != from.RepoPrefix {
			continue
		}
		if cand != nil {
			return "" // ambiguous — do not guess
		}
		cand = n
	}
	if cand == nil {
		return ""
	}
	return cand.ID
}
