package resolver

import (
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// Shared-stream candidate multiplexer for the framework synthesizers.
//
// On a cold / full-coverage run, the admission census already decodes the
// entire EdgeCalls stream once. Historically each via-gated synthesizer then
// re-decoded that same stream for itself — EdgeCalls was walked with full
// Meta decoding by eight-plus passes and EdgeReferences by four more, every
// walk discarding all but a tiny predicate-matched slice. The multiplexer
// collects each pass's candidate edges DURING the census walk instead: one
// decoded walk per edge kind feeds every consumer, and each pass receives
// its pre-matched slice at its own turn in the (unchanged) registry order.
//
// Only candidate COLLECTION is multiplexed. Pass logic, write ordering, and
// the sequential registry loop are untouched: a pass re-reads its collected
// candidates in their CURRENT store form before acting (see
// refetchFrameworkCandidates), so an edge retargeted by an earlier pass in
// the same run drops out exactly as it would vanish from a fresh stream walk.

// frameworkPassCandidateIdentities is the compact census-time buffer for one
// synthesizer. It retains only logical edge keys while the other passes run;
// payloads and Meta are fetched exactly when this pass is about to start.
type frameworkPassCandidateIdentities struct {
	calls     []graph.EdgeIdentity
	refs      []graph.EdgeIdentity
	annotated []graph.EdgeIdentity
	nodes     *frameworkNodeSnapshot
}

// frameworkPassCandidates is the live per-synthesizer bundle handed to a
// converted pass in place of its own whole-stream scans. It exists only for
// that pass invocation and reflects all mutations committed by earlier passes.
type frameworkPassCandidates struct {
	calls     []*graph.Edge
	refs      []*graph.Edge
	annotated []*graph.Edge
	// nodes is the run-wide shared node snapshot (one decoded walk per node
	// kind for the whole synthesizer loop).
	nodes *frameworkNodeSnapshot
}

// frameworkNodeSnapshot caches one materialised NodesByKind walk per node
// kind for the duration of a synthesizer run. Consumers that historically
// issued their own per-kind scans (temporal, rust, store-factory, macro)
// read the cached slice instead; single-kind order is preserved exactly, so
// each consumer sees the same nodes in the same order its own scan yielded.
type frameworkNodeSnapshot struct {
	byKind map[graph.NodeKind][]*graph.Node
}

// kind returns the cached slice for one node kind, materialising it on
// first use. Lazy per kind: a run whose admitted passes never consume a
// kind never pays its walk.
func (s *frameworkNodeSnapshot) kind(g graph.Store, kind graph.NodeKind) []*graph.Node {
	if s.byKind == nil {
		s.byKind = map[graph.NodeKind][]*graph.Node{}
	}
	if nodes, ok := s.byKind[kind]; ok {
		return nodes
	}
	var nodes []*graph.Node
	for n := range g.NodesByKind(kind) {
		if n != nil {
			nodes = append(nodes, n)
		}
	}
	s.byKind[kind] = nodes
	return nodes
}

// frameworkKindNodes returns a pass's node input for one kind: the shared
// snapshot slice when the pass runs in shared-stream form, else one direct
// kind scan (the legacy shape, kept for scoped runs and focused tests).
func frameworkKindNodes(g graph.Store, snap *frameworkNodeSnapshot, kind graph.NodeKind) []*graph.Node {
	if snap != nil {
		return snap.kind(g, kind)
	}
	var nodes []*graph.Node
	for n := range g.NodesByKind(kind) {
		if n != nil {
			nodes = append(nodes, n)
		}
	}
	return nodes
}

// frameworkStreamCandidates owns the armed collectors and the per-pass
// buffers for one full-census run.
type frameworkStreamCandidates struct {
	perPass map[string]*frameworkPassCandidateIdentities
	nodes   *frameworkNodeSnapshot

	// macroNames / macroIDs pre-filter the macro-expansion use-site arm.
	// Built from the node snapshot's Macro slice at construction (macro is
	// the one collector whose predicate needs node knowledge); a name-only
	// superset of the pass's own index, so it can only over-collect.
	macroNames map[string]struct{}
	macroIDs   map[string]struct{}

	callsCollectors []frameworkCandidateCollector
	refsCollectors  []frameworkCandidateCollector
	wantAnnotated   bool
}

// frameworkCandidateCollector pairs a pass with the cheap edge-only
// predicate mirroring that pass's own stream filter. A predicate is a
// necessary-condition superset: the pass re-applies its full filter over
// the (re-fetched) candidates, so over-collection is safe and
// under-collection is the only bug class.
type frameworkCandidateCollector struct {
	name string
	pred func(*graph.Edge) bool
}

// newFrameworkStreamCandidates arms a collector for every convertible pass
// whose family / node-marker gates pass on the census the light node walk
// just produced. Edge-preflight gates are census-derived and not yet known
// here; they can only narrow admission further, so the armed set is a
// superset of the passes that will run — an unconsumed buffer costs memory,
// never correctness.
func newFrameworkStreamCandidates(g graph.Store, present, markers map[string]int) *frameworkStreamCandidates {
	sc := &frameworkStreamCandidates{
		perPass: map[string]*frameworkPassCandidateIdentities{},
		nodes:   &frameworkNodeSnapshot{},
	}
	armed := func(name string) bool {
		return frameworkSynthNodeGatesPass(name, present, markers)
	}
	addCalls := func(name string, pred func(*graph.Edge) bool) {
		sc.callsCollectors = append(sc.callsCollectors, frameworkCandidateCollector{name: name, pred: pred})
	}
	addRefs := func(name string, pred func(*graph.Edge) bool) {
		sc.refsCollectors = append(sc.refsCollectors, frameworkCandidateCollector{name: name, pred: pred})
	}

	if armed(SynthGRPCStub) {
		addCalls(SynthGRPCStub, grpcCandidateEdge)
	}
	if armed(SynthTemporalStub) {
		addCalls(SynthTemporalStub, temporalCandidateEdge)
		sc.wantAnnotated = true
	}
	if armed(SynthStoreFactory) {
		addCalls(SynthStoreFactory, storeFactoryCandidateEdge)
	}
	if armed(SynthFnPointerDispatch) {
		addCalls(SynthFnPointerDispatch, fnPtrDispatchCandidateEdge)
		addRefs(SynthFnPointerDispatch, fnPtrRegCandidateEdge)
	}
	if armed(SynthMacroExpansion) {
		// The macro use-site predicate needs the function-like macro name
		// vocabulary; macros are a tiny kind, so warming the snapshot here
		// is the same walk the pass itself would have paid, done once.
		sc.macroNames = map[string]struct{}{}
		sc.macroIDs = map[string]struct{}{}
		for _, n := range sc.nodes.kind(g, graph.KindMacro) {
			if n == nil || n.Meta == nil || n.Name == "" {
				continue
			}
			if k, _ := n.Meta["macro_kind"].(string); k != macroFunctionKindMeta {
				continue
			}
			sc.macroNames[n.Name] = struct{}{}
			sc.macroIDs[n.ID] = struct{}{}
		}
		if len(sc.macroNames) > 0 {
			addCalls(SynthMacroExpansion, sc.macroCandidateEdge)
		}
	}
	if armed(SynthRailsResolve) {
		addCalls(SynthRailsResolve, railsCandidateEdge)
	}
	if armed(SynthReactResolve) {
		addCalls(SynthReactResolve, reactCandidateEdge)
		addRefs(SynthReactResolve, reactCandidateEdge)
	}
	if armed(SynthFastAPIResolve) {
		addCalls(SynthFastAPIResolve, fastapiCandidateEdge)
		addRefs(SynthFastAPIResolve, fastapiCandidateEdge)
	}
	if armed(SynthFactoryChain) {
		addCalls(SynthFactoryChain, factoryChainCandidateEdge)
		addRefs(SynthFactoryChain, factoryChainCandidateEdge)
	}
	if armed(SynthRustScope) {
		addCalls(SynthRustScope, rustCandidateEdge)
	}

	for _, c := range sc.callsCollectors {
		sc.ensurePass(c.name)
	}
	for _, c := range sc.refsCollectors {
		sc.ensurePass(c.name)
	}
	if sc.wantAnnotated {
		sc.ensurePass(SynthTemporalStub)
	}
	return sc
}

func (sc *frameworkStreamCandidates) ensurePass(name string) *frameworkPassCandidateIdentities {
	pc := sc.perPass[name]
	if pc == nil {
		pc = &frameworkPassCandidateIdentities{nodes: sc.nodes}
		sc.perPass[name] = pc
	}
	return pc
}

// passStreams materialises one pass's current edges with a single exact-key
// batch read. Stores without the optional exact lookup fall back to the legacy
// whole-stream form rather than issuing a broad adjacency query.
func (sc *frameworkStreamCandidates) passStreams(g graph.Store, name string) *frameworkPassCandidates {
	if sc == nil {
		return nil
	}
	return refetchFrameworkCandidates(g, sc.perPass[name])
}

// releasePass drops both the compact census buffer and the pass-local live
// edge slices as soon as the registry turn finishes. A skipped/gated pass has
// no live bundle, but its armed census buffer is still released here.
func (sc *frameworkStreamCandidates) releasePass(name string, bundle *frameworkPassCandidates) {
	if sc != nil {
		delete(sc.perPass, name)
	}
	if bundle == nil {
		return
	}
	bundle.calls = nil
	bundle.refs = nil
	bundle.annotated = nil
	bundle.nodes = nil
}

// collectCalls hands one census-walk EdgeCalls edge to every armed
// collector. Edges without a source node are skipped: no pass can act on a
// degenerate edge and the current-form re-read below is keyed by source.
func (sc *frameworkStreamCandidates) collectCalls(e *graph.Edge) {
	if sc == nil || e == nil || e.From == "" {
		return
	}
	for _, c := range sc.callsCollectors {
		if c.pred(e) {
			pc := sc.perPass[c.name]
			pc.calls = append(pc.calls, graph.EdgeIdentityFor(e))
		}
	}
}

// collectRefs is collectCalls for the EdgeReferences walk.
func (sc *frameworkStreamCandidates) collectRefs(e *graph.Edge) {
	if sc == nil || e == nil || e.From == "" {
		return
	}
	for _, c := range sc.refsCollectors {
		if c.pred(e) {
			pc := sc.perPass[c.name]
			pc.refs = append(pc.refs, graph.EdgeIdentityFor(e))
		}
	}
}

func (sc *frameworkStreamCandidates) wantsRefs() bool {
	return sc != nil && len(sc.refsCollectors) > 0
}

func (sc *frameworkStreamCandidates) wantsAnnotated() bool {
	return sc != nil && sc.wantAnnotated
}

func (sc *frameworkStreamCandidates) addAnnotated(e *graph.Edge) {
	if sc == nil || e == nil {
		return
	}
	pc := sc.perPass[SynthTemporalStub]
	pc.annotated = append(pc.annotated, graph.EdgeIdentityFor(e))
}

func (sc *frameworkStreamCandidates) annotatedCount() int {
	if sc == nil {
		return 0
	}
	if pc := sc.perPass[SynthTemporalStub]; pc != nil {
		return len(pc.annotated)
	}
	return 0
}

// refetchFrameworkCandidates re-reads one pass's CURRENT candidates in census
// order. A candidate retargeted or removed by an earlier pass is absent from
// the exact lookup and therefore drops out exactly as it would from a fresh
// kind scan. The lookup happens once, immediately before the pass begins.
func refetchFrameworkCandidates(g graph.Store, cands *frameworkPassCandidateIdentities) *frameworkPassCandidates {
	if cands == nil {
		return nil
	}
	finder, ok := g.(graph.EdgeIdentityBatchFinder)
	if !ok {
		return nil
	}
	identities := make([]graph.EdgeIdentity, 0, len(cands.calls)+len(cands.refs)+len(cands.annotated))
	identities = append(identities, cands.calls...)
	identities = append(identities, cands.refs...)
	identities = append(identities, cands.annotated...)
	current := finder.FindEdgesByIdentities(identities)
	materialize := func(ids []graph.EdgeIdentity) []*graph.Edge {
		out := make([]*graph.Edge, 0, len(ids))
		for _, identity := range ids {
			if edge := current[identity]; edge != nil {
				out = append(out, edge)
			}
		}
		return out
	}
	return &frameworkPassCandidates{
		calls: materialize(cands.calls), refs: materialize(cands.refs),
		annotated: materialize(cands.annotated), nodes: cands.nodes,
	}
}

// frameworkEdgeSeq adapts a candidate slice to the iterator shape a kind
// stream yields, so a pass's loop body is identical across the legacy and
// shared-stream forms.
func frameworkEdgeSeq(edges []*graph.Edge) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		for _, e := range edges {
			if !yield(e) {
				return
			}
		}
	}
}

// --- per-pass candidate predicates -------------------------------------
//
// Each predicate is a verbatim copy of the cheap edge-only prefix of its
// pass's own stream filter. Conditions needing graph reads (source-file
// language, node lookups, index membership) stay in the pass, which
// re-applies its full filter over the re-fetched candidates.

func grpcCandidateEdge(e *graph.Edge) bool {
	if e.Meta == nil {
		return false
	}
	// Both arms of the pass read EdgeCalls: the stub arm keys on the via,
	// the handler-index arm on the registration marker.
	if v, _ := e.Meta["via"].(string); v == "grpc.stub" {
		return true
	}
	svc, _ := e.Meta["grpc_register_service"].(string)
	return svc != ""
}

func temporalCandidateEdge(e *graph.Edge) bool {
	if e.Meta == nil {
		return false
	}
	// The prefix covers every phase input: register / stub / start for the
	// sweep, executor-field markers for the pre-pass, handler edges for the
	// cross-language join — mirroring the pass's own presence probe.
	v, _ := e.Meta["via"].(string)
	return strings.HasPrefix(v, "temporal.")
}

func storeFactoryCandidateEdge(e *graph.Edge) bool {
	if e.Meta == nil {
		return false
	}
	v, _ := e.Meta["via"].(string)
	return v == storeFactoryVia
}

func fnPtrDispatchCandidateEdge(e *graph.Edge) bool {
	if e.Meta == nil {
		return false
	}
	v, _ := e.Meta["via"].(string)
	return v == fnPtrDispatchVia
}

func fnPtrRegCandidateEdge(e *graph.Edge) bool {
	if e.Meta == nil {
		return false
	}
	v, _ := e.Meta["via"].(string)
	return v == fnPtrRegVia
}

func (sc *frameworkStreamCandidates) macroCandidateEdge(e *graph.Edge) bool {
	if e.To == "" {
		return false
	}
	if graph.IsUnresolvedTarget(e.To) {
		_, ok := sc.macroNames[graph.UnresolvedName(e.To)]
		return ok
	}
	_, ok := sc.macroIDs[e.To]
	return ok
}

func railsCandidateEdge(e *graph.Edge) bool {
	if e.Meta == nil || !graph.IsUnresolvedTarget(e.To) {
		return false
	}
	recv, _ := e.Meta["recv_const"].(string)
	return recv != ""
}

func reactCandidateEdge(e *graph.Edge) bool {
	if !graph.IsUnresolvedTarget(e.To) {
		return false
	}
	head := graph.UnresolvedName(e.To)
	if i := strings.IndexByte(head, '.'); i >= 0 {
		head = head[:i]
	}
	via, _ := e.Meta["via"].(string)
	_, _, ok := reactResolveShape(head, via)
	return ok
}

func fastapiCandidateEdge(e *graph.Edge) bool {
	if e.Meta == nil || !graph.IsUnresolvedTarget(e.To) {
		return false
	}
	switch v, _ := e.Meta["via"].(string); v {
	case "fastapi.Depends", "fastapi.router":
		return true
	}
	return false
}

func factoryChainCandidateEdge(e *graph.Edge) bool {
	if e.Meta == nil || !graph.IsUnresolvedTarget(e.To) {
		return false
	}
	expr, _ := e.Meta["receiver_expr"].(string)
	return expr != ""
}

func rustCandidateEdge(e *graph.Edge) bool {
	return graph.IsUnresolvedTarget(e.To) && rustScopeEdgeCandidate(e)
}
