package resolver

import "github.com/zzet/gortex/internal/graph"

const (
	postBareAttributionCensusMaxCandidates    = 256 << 10
	postBareAttributionCensusMaxRetainedBytes = int64(64 << 20)
	// EdgeIdentity currently carries four string headers and one int. Include
	// padding plus a small slice/accounting allowance, then add the retained
	// string bytes below. The estimate deliberately over-counts in-memory
	// stores (their strings are already graph-owned) so the cap always fails
	// toward the exact legacy scans, never toward another heap spike.
	attributionCensusIdentityOverhead = int64(80)
)

var postBareAttributionCensusKinds = []graph.EdgeKind{
	graph.EdgeCalls,
	graph.EdgeReferences,
	graph.EdgeArgOf,
	graph.EdgeValueFlow,
	graph.EdgeTypedAs,
	graph.EdgeReturns,
	graph.EdgeInstantiates,
}

type attributionCensusLimits struct {
	maxCandidates    int
	maxRetainedBytes int64
}

var defaultAttributionCensusLimits = attributionCensusLimits{
	maxCandidates:    postBareAttributionCensusMaxCandidates,
	maxRetainedBytes: postBareAttributionCensusMaxRetainedBytes,
}

type postBareAttributionCensus struct {
	finder             graph.EdgeIdentityBatchFinder
	callee             *calleeIndex
	dataflowCandidates []graph.EdgeIdentity
	genericCandidates  []graph.EdgeIdentity
	retainedBytes      int64
}

type attributionCensusStats struct {
	rowsScanned        int
	dataflowCandidates int
	genericCandidates  int
	retainedBytes      int64
	fallbackReason     string
}

// buildPostBareAttributionCensus replaces the six corpus reads behind the
// dataflow and generic-parameter passes with one metadata-free kind-union walk.
// It runs after bare-name attribution, so references that pass just resolved
// already contribute their current call-site target to callee.bySite. Only
// compact logical identities survive the walk; each pass exact-refetches its
// current rows in bounded pages immediately before mutation.
//
// This deliberately does not share Resolver.ResolveAll's unresolved pages
// with CrossRepoResolver. The master compute, guard, and attribution tails can
// retarget or revert those rows before cross-repo resolution begins, while an
// interleaving writer can add a new unresolved row between the two passes.
// Retaining the raw pages would also recreate the whole-frontier heap spike.
// A safe cross-resolver hand-off therefore needs a store-owned, revision-bound,
// disk-resident frontier and is a separate change.
func (r *Resolver) buildPostBareAttributionCensus(
	limits attributionCensusLimits,
) (*postBareAttributionCensus, attributionCensusStats) {
	var stats attributionCensusStats
	light, lightOK := r.graph.(graph.LightEdgeSequencer)
	finder, finderOK := r.graph.(graph.EdgeIdentityBatchFinder)
	if !lightOK || !finderOK {
		stats.fallbackReason = "missing_projection_capability"
		return nil, stats
	}
	if limits.maxCandidates <= 0 || limits.maxRetainedBytes <= 0 {
		stats.fallbackReason = "invalid_limit"
		return nil, stats
	}

	census := &postBareAttributionCensus{
		finder: finder,
		callee: newCalleeIndex(),
	}
	appendCandidate := func(dst *[]graph.EdgeIdentity, edge *graph.Edge) bool {
		identity := graph.EdgeIdentityFor(edge)
		retained := attributionCensusIdentityBytes(identity)
		count := len(census.dataflowCandidates) + len(census.genericCandidates)
		if count >= limits.maxCandidates {
			stats.fallbackReason = "candidate_cap"
			return false
		}
		if census.retainedBytes+retained > limits.maxRetainedBytes {
			stats.fallbackReason = "retained_byte_cap"
			return false
		}
		*dst = append(*dst, identity)
		census.retainedBytes += retained
		return true
	}

	complete := true
	for edge := range light.EdgesLightSeq(postBareAttributionCensusKinds...) {
		if edge == nil {
			continue
		}
		stats.rowsScanned++
		switch edge.Kind {
		case graph.EdgeCalls, graph.EdgeReferences:
			census.callee.indexCallSite(edge)
		}
		switch edge.Kind {
		case graph.EdgeArgOf, graph.EdgeValueFlow:
			if dataflowCalleeTargetShape(edge.To) {
				complete = appendCandidate(&census.dataflowCandidates, edge)
			}
		case graph.EdgeReferences, graph.EdgeTypedAs, graph.EdgeReturns, graph.EdgeInstantiates:
			if _, ok := genericParamTargetName(edge.To); ok {
				complete = appendCandidate(&census.genericCandidates, edge)
			}
		}
		if !complete {
			break
		}
	}

	stats.dataflowCandidates = len(census.dataflowCandidates)
	stats.genericCandidates = len(census.genericCandidates)
	stats.retainedBytes = census.retainedBytes
	if !complete {
		census.close()
		return nil, stats
	}
	// The callable-node projection can be large. Defer it until candidate
	// admission succeeds so a capped census does not build this index only for
	// the exact legacy fallback to build it again moments later. Call-site
	// evidence still comes from the single light edge walk above.
	callSites := census.callee.bySite
	census.callee = r.buildCalleeIndex()
	census.callee.bySite = callSites
	return census, stats
}

func attributionCensusIdentityBytes(identity graph.EdgeIdentity) int64 {
	return attributionCensusIdentityOverhead + int64(
		len(identity.From)+len(identity.To)+len(identity.Kind)+len(identity.FilePath),
	)
}

func (c *postBareAttributionCensus) releaseDataflow() {
	if c == nil {
		return
	}
	c.dataflowCandidates = nil
	c.callee = nil
}

func (c *postBareAttributionCensus) releaseGeneric() {
	if c == nil {
		return
	}
	c.genericCandidates = nil
}

func (c *postBareAttributionCensus) close() {
	if c == nil {
		return
	}
	c.releaseDataflow()
	c.releaseGeneric()
	c.finder = nil
}
