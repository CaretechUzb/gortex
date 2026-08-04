package resolver

import (
	"sort"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// reactSetStateVia marks a synthesized React class-component setState→render
// reachability edge.
const reactSetStateVia = "react.setstate"

// ResolveReactSetStateCalls is the framework-dispatch synthesizer for the React
// class-component re-render hop. `this.setState(...)` re-runs the component's
// `render()`, but that hop is React-internal — no static edge — so a flow like
// "event → setState → render → child components" dead-ends at setState even
// though everything after render is call-connected. This pass bridges it: for
// each class that has a `render` method, it links every sibling method whose
// body calls `this.setState(` to that `render`. The setState call is the gate
// that keeps this to React class components — a plain class with a `render`
// method that never calls `this.setState` produces no edge.
//
// Over-approximation by design (every setState method reaches render), full
// recompute and idempotent: edges are re-derived from the call + membership
// metadata, graph.AddEdge dedupes, and graph.EvictFile drops them on reindex.
// Edges ride at ast_inferred and carry synthesizer provenance.
//
// Returns the number of setState→render edges synthesized.
func ResolveReactSetStateCalls(g graph.Store) int {
	return resolveSetStateLifecycleCalls(g, "render", reactSetStateEdge)
}

type setStateLifecycleEdgeBuilder func(from, target *graph.Node, class string) *graph.Edge

// resolveSetStateLifecycleCalls performs the shared React/Flutter join without
// decoding every method node and every outgoing method edge. The membership
// projection is the cheap census; only methods belonging to a class with the
// requested lifecycle method reach the adjacency lookup.
func resolveSetStateLifecycleCalls(
	g graph.Store,
	lifecycleName string,
	edgeBuilder setStateLifecycleEdgeBuilder,
) int {
	if g == nil || edgeBuilder == nil {
		return 0
	}

	methodsByClass := memberMethodInfosByType(g)
	lifecycleClasses := make(map[string]struct{})
	for classID, methods := range methodsByClass {
		if classID == "" {
			continue
		}
		for _, method := range methods {
			if method.MethodID != "" && method.Name == lifecycleName {
				lifecycleClasses[classID] = struct{}{}
				break
			}
		}
	}
	if len(lifecycleClasses) == 0 {
		return 0
	}

	// The projection can list one method under more than one type. Deduplicate
	// before the store lookup; the targeted adjacency below still selects the
	// first persisted member_of edge, preserving the legacy ownership rule.
	candidateByID := make(map[string]graph.MemberMethodInfo)
	for classID := range lifecycleClasses {
		for _, method := range methodsByClass[classID] {
			if method.MethodID == "" {
				continue
			}
			candidateByID[method.MethodID] = method
		}
	}
	candidateIDs := make([]string, 0, len(candidateByID))
	for methodID := range candidateByID {
		candidateIDs = append(candidateIDs, methodID)
	}
	sort.Strings(candidateIDs)
	outByMethod := g.GetOutEdgesByNodeIDs(candidateIDs)

	classByMethod := make(map[string]string, len(candidateIDs))
	for _, methodID := range candidateIDs {
		for _, edge := range outByMethod[methodID] {
			if edge == nil || edge.Kind != graph.EdgeMemberOf {
				continue
			}
			classByMethod[methodID] = edge.To
			break
		}
	}

	// candidateIDs are sorted exactly like SQLite's former KindMethod scan, so
	// duplicate lifecycle methods retain its stable last-writer behavior.
	targetByClass := make(map[string]*graph.Node)
	for _, methodID := range candidateIDs {
		method := candidateByID[methodID]
		if method.Name != lifecycleName {
			continue
		}
		classID := classByMethod[methodID]
		if classID == "" {
			continue
		}
		targetByClass[classID] = memberMethodInfoNode(method)
	}
	if len(targetByClass) == 0 {
		return 0
	}

	batch := make([]*graph.Edge, 0)
	for _, methodID := range candidateIDs {
		method := candidateByID[methodID]
		classID := classByMethod[methodID]
		target := targetByClass[classID]
		if target == nil || target.ID == methodID || !edgesCallSetState(outByMethod[methodID]) {
			continue
		}
		batch = append(batch, edgeBuilder(memberMethodInfoNode(method), target, classID))
	}
	if len(batch) > 0 {
		g.AddBatch(nil, batch)
	}
	return len(batch)
}

func memberMethodInfoNode(method graph.MemberMethodInfo) *graph.Node {
	return &graph.Node{
		ID:         method.MethodID,
		Kind:       graph.KindMethod,
		Name:       method.Name,
		FilePath:   method.FilePath,
		StartLine:  method.StartLine,
		RepoPrefix: method.RepoPrefix,
	}
}

func edgesCallSetState(edges []*graph.Edge) bool {
	for _, e := range edges {
		if e == nil || e.Kind != graph.EdgeCalls {
			continue
		}
		if isSetStateTarget(e.To) {
			return true
		}
	}
	return false
}

// isSetStateTarget matches a call target that names React's setState.
func isSetStateTarget(to string) bool {
	return strings.HasSuffix(to, ".setState") ||
		strings.HasSuffix(to, "::setState") ||
		to == "unresolved::setState"
}

// reactSetStateEdge builds one setState-method → render synthesized edge.
func reactSetStateEdge(from, render *graph.Node, class string) *graph.Edge {
	return &graph.Edge{
		From:            from.ID,
		To:              render.ID,
		Kind:            graph.EdgeCalls,
		FilePath:        from.FilePath,
		Line:            from.StartLine,
		Confidence:      0.6,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, 0.6),
		Origin:          graph.OriginASTInferred,
		Meta: map[string]any{
			"via":             reactSetStateVia,
			"component_class": class,
			MetaSynthesizedBy: SynthReactSetState,
			MetaProvenance:    ProvenanceHeuristic,
		},
	}
}
