package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type placeholderCandidateTrackingStore struct {
	*Graph
	candidateCalls int
	adjacencyCalls int
	sites          []EdgeSite
}

func (s *placeholderCandidateTrackingStore) GetEdgeCandidates(endpoints []EdgeEndpoint, sites []EdgeSite) EdgeCandidateSet {
	s.candidateCalls++
	s.sites = append(s.sites, sites...)
	return s.Graph.GetEdgeCandidates(endpoints, sites)
}

func (s *placeholderCandidateTrackingStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*Edge {
	s.adjacencyCalls++
	return s.Graph.GetOutEdgesByNodeIDs(ids)
}

// Resolving a placeholder reference at a site must re-point the dataflow
// edges keyed FROM that placeholder at the SAME site only — placeholder
// strings are shared across sites, and a different-line sibling must stay
// pending for its own resolution.
func TestReconcilePlaceholderSourcesExactSite(t *testing.T) {
	g := New()
	tracked := &placeholderCandidateTrackingStore{Graph: g}
	// Mirrors production exactly: the reference target stays in the BARE
	// unresolved form while applyRepoPrefix gave the dataflow sources the
	// repo-slash-prefixed form — two strings for one conceptual placeholder.
	const (
		caller         = "repo/a.go::Caller"
		callee         = "repo/b.go::Callee"
		resolved       = "repo/c.go::Field"
		bareTarget     = "unresolved::*.id"
		prefixedSource = "repo/unresolved::*.id"
	)
	ref := &Edge{From: caller, To: bareTarget, Kind: EdgeReferences, FilePath: "repo/a.go", Line: 7}
	sameSite := &Edge{From: prefixedSource, To: callee, Kind: EdgeArgOf, FilePath: "repo/a.go", Line: 7}
	otherSite := &Edge{From: prefixedSource, To: callee, Kind: EdgeValueFlow, FilePath: "repo/a.go", Line: 9}
	otherKind := &Edge{From: prefixedSource, To: callee, Kind: EdgeReferences, FilePath: "repo/a.go", Line: 7}
	g.AddBatch(nil, []*Edge{ref, sameSite, otherSite, otherKind})

	// The resolver rewrites the reference target, then reconciles sources
	// from the same reindex batch.
	oldTo := ref.To
	ref.To = resolved
	batch := []EdgeReindex{{Edge: ref, OldTo: oldTo}}
	g.ReindexEdges(batch)

	repoints := PlaceholderSourceRepoints(batch)
	require.Len(t, repoints, 2, "bare and repo-prefixed source forms are both candidates")
	moved := ReconcilePlaceholderSources(tracked, repoints)
	assert.Equal(t, 1, moved)
	assert.Equal(t, 1, tracked.candidateCalls)
	assert.Zero(t, tracked.adjacencyCalls, "reconciliation must not decode full placeholder adjacency")
	assert.Len(t, tracked.sites, 4, "two source forms times two eligible edge kinds")

	fromResolved := g.GetOutEdges(resolved)
	require.Len(t, fromResolved, 1, "same-site dataflow edge must move under the resolved node")
	assert.Equal(t, EdgeArgOf, fromResolved[0].Kind)
	assert.Equal(t, callee, fromResolved[0].To)

	remaining := g.GetOutEdges(prefixedSource)
	require.Len(t, remaining, 2, "other-site and non-dataflow edges must stay pending")
	kinds := map[EdgeKind]int{}
	for _, e := range remaining {
		kinds[e.Kind]++
	}
	assert.Equal(t, 1, kinds[EdgeValueFlow])
	assert.Equal(t, 1, kinds[EdgeReferences])
}

// A batch with no placeholder→real rewrites must extract nothing: still-
// unresolved rewrites and already-real old targets are both ineligible.
func TestPlaceholderSourceRepointsEligibility(t *testing.T) {
	realEdge := &Edge{From: "a", To: "repo/x.go::N", Kind: EdgeCalls, FilePath: "f", Line: 1}
	stillUnresolved := &Edge{From: "a", To: "repo/unresolved::Y", Kind: EdgeCalls, FilePath: "f", Line: 2}
	batch := []EdgeReindex{
		{Edge: realEdge, OldTo: "repo/x.go::Old"},            // real → real: not a placeholder resolution
		{Edge: stillUnresolved, OldTo: "repo/unresolved::X"}, // placeholder → placeholder: nothing to bind
		{Edge: nil, OldTo: "repo/unresolved::Z"},
	}
	assert.Empty(t, PlaceholderSourceRepoints(batch))
}
