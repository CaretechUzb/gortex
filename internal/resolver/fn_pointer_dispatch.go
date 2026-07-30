package resolver

import (
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// fnPtrDispatchVia marks a C/C++ indirect-dispatch placeholder
// (`cmds[i].fn(...)`); fnPtrRegVia marks a registration carrier (a concrete
// function bound to a struct's fn-pointer field). Both must match the
// extractor's constants.
const (
	fnPtrDispatchVia = "fn-pointer-dispatch"
	fnPtrRegVia      = "fn-pointer-reg"
)

// fnPointerFanoutCap bounds the functions a single dispatch slot may fan out
// to. fnPointerConfidence is the struct+field-keyed confidence — higher than
// a pure-name guess, lower than a typed binding.
const (
	fnPointerFanoutCap  = 64
	fnPointerConfidence = 0.7
)

// ResolveFnPointerDispatch binds C/C++ function-pointer dispatch: a function
// registered into a struct's fn-pointer field
// (`struct cmd cmds[] = {{"add", cmd_add}}`) is linked to the indirect call
// `cmds[i].fn(...)` whose receiver resolves to that struct type. Registrations
// (positional + designated initializers, `x.field = fn` assignments, and
// `a.field = b.field` copies propagated to fixpoint) build a (struct, field)
// → {functions} index; each dispatch site fans out to every function in its
// slot.
//
// Returns the number of dispatcher → function edges synthesized.
func ResolveFnPointerDispatch(g graph.Store) int {
	return resolveFnPointerDispatch(g, nil)
}

// resolveFnPointerDispatch is the census-multiplexed form: cands, when
// non-nil, replaces both whole-stream decodes — the EdgeReferences
// registration arm and the EdgeCalls dispatch arm — with the shared walk's
// pre-matched candidates.
func resolveFnPointerDispatch(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}
	slotFns := map[string]map[string]*graph.Node{}
	addFn := func(key string, n *graph.Node) {
		if n == nil {
			return
		}
		if slotFns[key] == nil {
			slotFns[key] = map[string]*graph.Node{}
		}
		slotFns[key][n.ID] = n
	}
	type copyEdge struct{ to, from string }
	var copies []copyEdge

	regStream := g.EdgesByKind(graph.EdgeReferences)
	if cands != nil {
		regStream = frameworkEdgeSeq(cands.refs)
	}
	var regReindex []graph.EdgeReindex
	for e := range regStream {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != fnPtrRegVia {
			continue
		}
		st, _ := e.Meta["fnptr_struct"].(string)
		field, _ := e.Meta["fnptr_field"].(string)
		if st == "" || field == "" {
			continue
		}
		key := st + "\x00" + field
		if cs, _ := e.Meta["fnptr_copy_struct"].(string); cs != "" {
			cf, _ := e.Meta["fnptr_copy_field"].(string)
			copies = append(copies, copyEdge{to: key, from: cs + "\x00" + cf})
			continue
		}
		fn, _ := e.Meta["fnptr_fn"].(string)
		if fn == "" {
			continue
		}
		target := fnPtrFunctionByName(g, e, fn)
		if target == nil {
			continue
		}
		addFn(key, target)
		if e.To != target.ID {
			oldTo := e.To
			e.To = target.ID
			e.Origin = graph.OriginASTInferred
			regReindex = append(regReindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
		}
	}
	if len(regReindex) > 0 {
		g.ReindexEdges(regReindex)
	}
	if len(slotFns) == 0 {
		return 0
	}

	// Field←field propagation fixpoint.
	for iter := 0; iter < 8; iter++ {
		changed := false
		for _, c := range copies {
			for id, n := range slotFns[c.from] {
				if slotFns[c.to] == nil {
					slotFns[c.to] = map[string]*graph.Node{}
				}
				if _, ok := slotFns[c.to][id]; !ok {
					slotFns[c.to][id] = n
					changed = true
				}
			}
		}
		if !changed {
			break
		}
	}

	dispatchStream := g.EdgesByKind(graph.EdgeCalls)
	if cands != nil {
		dispatchStream = frameworkEdgeSeq(cands.calls)
	}
	resolved := 0
	var reindex []graph.EdgeReindex
	var batch []*graph.Edge
	for e := range dispatchStream {
		if e == nil || e.Meta == nil {
			continue
		}
		if v, _ := e.Meta["via"].(string); v != fnPtrDispatchVia {
			continue
		}
		st, _ := e.Meta["fnptr_struct"].(string)
		field, _ := e.Meta["fnptr_field"].(string)
		if st == "" || field == "" {
			continue
		}
		fns := fnPtrSortedSlot(slotFns[st+"\x00"+field])
		if len(fns) > fnPointerFanoutCap {
			fns = fns[:fnPointerFanoutCap]
		}
		if len(fns) == 0 {
			resolved += fnPtrRebind(e, nil, &reindex)
			continue
		}
		resolved += fnPtrRebind(e, fns[0], &reindex)
		for _, n := range fns[1:] {
			batch = append(batch, fnPtrFanoutEdge(e, n, st, field))
			resolved++
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	for _, ne := range batch {
		g.AddEdge(ne)
	}
	return resolved
}

func fnPtrRebind(e *graph.Edge, target *graph.Node, reindex *[]graph.EdgeReindex) int {
	field, _ := e.Meta["fnptr_field"].(string)
	want := "unresolved::*." + field
	if target != nil {
		want = target.ID
	}
	if e.To == want {
		if target != nil {
			return 1
		}
		return 0
	}
	oldTo := e.To
	e.To = want
	hit := 0
	if target != nil {
		e.Origin = graph.OriginASTInferred
		e.Confidence = fnPointerConfidence
		e.ConfidenceLabel = graph.ConfidenceLabelFor(graph.EdgeCalls, fnPointerConfidence)
		StampSynthesized(e, SynthFnPointerDispatch)
		hit = 1
	} else {
		e.Origin = graph.OriginASTInferred
		e.Confidence = 0
		e.ConfidenceLabel = ""
		UnstampSynthesized(e)
	}
	*reindex = append(*reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
	return hit
}

func fnPtrFanoutEdge(e *graph.Edge, target *graph.Node, st, field string) *graph.Edge {
	return &graph.Edge{
		From: e.From, To: target.ID, Kind: graph.EdgeCalls,
		FilePath: e.FilePath, Line: e.Line,
		Origin:          graph.OriginASTInferred,
		Confidence:      fnPointerConfidence,
		ConfidenceLabel: graph.ConfidenceLabelFor(graph.EdgeCalls, fnPointerConfidence),
		Meta: map[string]any{
			"via":             fnPtrDispatchVia,
			"fnptr_struct":    st,
			"fnptr_field":     field,
			MetaSynthesizedBy: SynthFnPointerDispatch,
			MetaProvenance:    ProvenanceHeuristic,
		},
	}
}

// fnPtrFunctionByName resolves a registered function name to its node,
// preferring the same file as the registration, then a unique match.
func fnPtrFunctionByName(g graph.Store, reg *graph.Edge, name string) *graph.Node {
	var cands []*graph.Node
	for _, n := range g.FindNodesByName(name) {
		if n == nil || (n.Kind != graph.KindFunction && n.Kind != graph.KindMethod) {
			continue
		}
		if graph.IsStub(n.ID) || graph.IsUnresolvedTarget(n.ID) {
			continue
		}
		cands = append(cands, n)
	}
	return pickStoreAction(g, reg, sameBoundaryCandidates(g, reg.From, cands))
}

func fnPtrSortedSlot(m map[string]*graph.Node) []*graph.Node {
	out := make([]*graph.Node, 0, len(m))
	for _, n := range m {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
