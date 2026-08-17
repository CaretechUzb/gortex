package graph

import (
	"context"
	"sort"
)

const (
	// MaxBoundedAdjacencyKeys bounds both endpoint and source-site batches.
	MaxBoundedAdjacencyKeys = 256
	// MaxBoundedAdjacencyKinds bounds the raw requested kind set.
	MaxBoundedAdjacencyKinds = 16
	// MaxBoundedAdjacencyRowsPerKey bounds complete identities retained for one key.
	MaxBoundedAdjacencyRowsPerKey = 256
	// MaxBoundedAdjacencyTotalIdentities bounds matching identities inspected
	// across one projection, including each limit+1 truncation sentinel.
	MaxBoundedAdjacencyTotalIdentities = 16_384
	// MaxBoundedAdjacencyInspectedEdges bounds raw adjacency slots per physical
	// reader layer. An OverlaidView makes at most one bounded base call and one
	// separately capped local scan; matching base and local identities are
	// recharged against MaxBoundedAdjacencyTotalIdentities before return.
	MaxBoundedAdjacencyInspectedEdges = 16_384
)

// EdgeSourceSite identifies one source line. FilePath deliberately is not part of the
// key: source-literal attribution historically keys call sites by graph source
// identity and line, while each returned EdgeIdentity retains its provenance.
type EdgeSourceSite struct {
	From string
	Line int
}

// BoundedEdgeIdentityProjection is a metadata-free adjacency projection keyed
// by source (outgoing) or target (incoming) ID. A truncated key has no retained
// identities; callers must fail closed for that key.
type BoundedEdgeIdentityProjection struct {
	ByEndpoint map[string][]EdgeIdentity
	Truncated  map[string]bool
}

// BoundedSiteEdgeIdentityProjection is the exact-source-site sibling.
type BoundedSiteEdgeIdentityProjection struct {
	BySite    map[EdgeSourceSite][]EdgeIdentity
	Truncated map[EdgeSourceSite]bool
}

// BoundedOutgoingEdgeIdentityReader is an optional metadata-free projection.
type BoundedOutgoingEdgeIdentityReader interface {
	FindOutgoingEdgeIdentitiesBounded(context.Context, []string, []EdgeKind, int) (BoundedEdgeIdentityProjection, error)
}

// BoundedIncomingEdgeIdentityReader is the inbound sibling.
type BoundedIncomingEdgeIdentityReader interface {
	FindIncomingEdgeIdentitiesBounded(context.Context, []string, []EdgeKind, int) (BoundedEdgeIdentityProjection, error)
}

// BoundedOutgoingSiteEdgeIdentityReader projects exact source-line adjacency.
type BoundedOutgoingSiteEdgeIdentityReader interface {
	FindOutgoingSiteEdgeIdentitiesBounded(context.Context, []EdgeSourceSite, []EdgeKind, int) (BoundedSiteEdgeIdentityProjection, error)
}

var (
	_ BoundedOutgoingEdgeIdentityReader     = (*Graph)(nil)
	_ BoundedIncomingEdgeIdentityReader     = (*Graph)(nil)
	_ BoundedOutgoingSiteEdgeIdentityReader = (*Graph)(nil)
)

func emptyBoundedEdgeIdentityProjection() BoundedEdgeIdentityProjection {
	return BoundedEdgeIdentityProjection{
		ByEndpoint: make(map[string][]EdgeIdentity),
		Truncated:  make(map[string]bool),
	}
}

func emptyBoundedSiteEdgeIdentityProjection() BoundedSiteEdgeIdentityProjection {
	return BoundedSiteEdgeIdentityProjection{
		BySite:    make(map[EdgeSourceSite][]EdgeIdentity),
		Truncated: make(map[EdgeSourceSite]bool),
	}
}

func validateBoundedAdjacencyLimit(limit int) error {
	if limit < 1 || limit > MaxBoundedAdjacencyRowsPerKey {
		return &BoundedLocalizationLimitError{
			Resource: "adjacency rows per key",
			Limit:    MaxBoundedAdjacencyRowsPerKey,
		}
	}
	return nil
}

func canonicalBoundedAdjacencyKinds(ctx context.Context, kinds []EdgeKind) (map[EdgeKind]struct{}, error) {
	if len(kinds) > MaxBoundedAdjacencyKinds {
		return nil, &BoundedLocalizationLimitError{
			Resource: "adjacency kinds",
			Limit:    MaxBoundedAdjacencyKinds,
		}
	}
	out := make(map[EdgeKind]struct{}, len(kinds))
	for index, kind := range kinds {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if kind != "" {
			out[kind] = struct{}{}
		}
	}
	return out, nil
}

func canonicalBoundedAdjacencyEndpoints(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) > MaxBoundedAdjacencyKeys {
		return nil, &BoundedLocalizationLimitError{
			Resource: "adjacency endpoint keys",
			Limit:    MaxBoundedAdjacencyKeys,
		}
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for index, id := range ids {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func canonicalBoundedAdjacencySites(ctx context.Context, sites []EdgeSourceSite) ([]EdgeSourceSite, error) {
	if len(sites) > MaxBoundedAdjacencyKeys {
		return nil, &BoundedLocalizationLimitError{
			Resource: "adjacency source-site keys",
			Limit:    MaxBoundedAdjacencyKeys,
		}
	}
	seen := make(map[EdgeSourceSite]struct{}, len(sites))
	out := make([]EdgeSourceSite, 0, len(sites))
	for index, site := range sites {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if site.From == "" {
			continue
		}
		if _, duplicate := seen[site]; duplicate {
			continue
		}
		seen[site] = struct{}{}
		out = append(out, site)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

func sortEdgeIdentities(identities []EdgeIdentity) {
	sort.Slice(identities, func(i, j int) bool {
		if identities[i].From != identities[j].From {
			return identities[i].From < identities[j].From
		}
		if identities[i].To != identities[j].To {
			return identities[i].To < identities[j].To
		}
		if identities[i].Kind != identities[j].Kind {
			return identities[i].Kind < identities[j].Kind
		}
		if identities[i].FilePath != identities[j].FilePath {
			return identities[i].FilePath < identities[j].FilePath
		}
		return identities[i].Line < identities[j].Line
	})
}

type boundedAdjacencyBudget struct {
	inspected int
	relevant  int
}

func (budget *boundedAdjacencyBudget) inspect() error {
	if budget.inspected == MaxBoundedAdjacencyInspectedEdges {
		return &BoundedLocalizationLimitError{
			Resource: "adjacency inspected edge rows",
			Limit:    MaxBoundedAdjacencyInspectedEdges,
		}
	}
	budget.inspected++
	return nil
}

func (budget *boundedAdjacencyBudget) retain() error {
	if budget.relevant == MaxBoundedAdjacencyTotalIdentities {
		return &BoundedLocalizationLimitError{
			Resource: "adjacency matching identities",
			Limit:    MaxBoundedAdjacencyTotalIdentities,
		}
	}
	budget.relevant++
	return nil
}

func scanBoundedEdgeIdentities(
	ctx context.Context,
	edges []*Edge,
	kinds map[EdgeKind]struct{},
	limit int,
	site *EdgeSourceSite,
	budget *boundedAdjacencyBudget,
	accept func(EdgeIdentity) bool,
) ([]EdgeIdentity, bool, error) {
	var identities []EdgeIdentity
	var seen map[EdgeIdentity]struct{}
	for index, edge := range edges {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		if err := budget.inspect(); err != nil {
			return nil, false, err
		}
		if edge == nil {
			continue
		}
		if site != nil && (edge.From != site.From || edge.Line != site.Line) {
			continue
		}
		if _, requested := kinds[edge.Kind]; !requested {
			continue
		}
		identity := EdgeIdentityFor(edge)
		if accept != nil && !accept(identity) {
			continue
		}
		if seen == nil {
			identities = make([]EdgeIdentity, 0, 1)
			seen = make(map[EdgeIdentity]struct{}, 1)
		}
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		if err := budget.retain(); err != nil {
			return nil, false, err
		}
		if len(identities) == limit {
			return nil, true, nil
		}
		identities = append(identities, identity)
	}
	sortEdgeIdentities(identities)
	return identities, false, nil
}

type boundedSiteScanState struct {
	site       EdgeSourceSite
	identities []EdgeIdentity
	seen       map[EdgeIdentity]struct{}
	truncated  bool
}

// scanBoundedSiteEdgeIdentities scans one source adjacency exactly once and
// dispatches matching rows to every requested line for that source. Raw work is
// therefore charged per physical adjacency slot, not once per requested site.
func scanBoundedSiteEdgeIdentities(
	ctx context.Context,
	edges []*Edge,
	sites []EdgeSourceSite,
	kinds map[EdgeKind]struct{},
	limit int,
	budget *boundedAdjacencyBudget,
	accept func(EdgeIdentity) bool,
) (map[EdgeSourceSite][]EdgeIdentity, map[EdgeSourceSite]bool, error) {
	states := make(map[int]*boundedSiteScanState, len(sites))
	for _, site := range sites {
		states[site.Line] = &boundedSiteScanState{site: site}
	}
	active := len(states)
	for index, edge := range edges {
		if active == 0 {
			break
		}
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
		}
		if err := budget.inspect(); err != nil {
			return nil, nil, err
		}
		if edge == nil {
			continue
		}
		state := states[edge.Line]
		if state == nil || state.truncated {
			continue
		}
		if _, requested := kinds[edge.Kind]; !requested {
			continue
		}
		identity := EdgeIdentityFor(edge)
		if accept != nil && !accept(identity) {
			continue
		}
		if state.seen == nil {
			state.identities = make([]EdgeIdentity, 0, 1)
			state.seen = make(map[EdgeIdentity]struct{}, 1)
		}
		if _, duplicate := state.seen[identity]; duplicate {
			continue
		}
		state.seen[identity] = struct{}{}
		if err := budget.retain(); err != nil {
			return nil, nil, err
		}
		if len(state.identities) == limit {
			state.identities = nil
			state.truncated = true
			active--
			continue
		}
		state.identities = append(state.identities, identity)
	}
	bySite := make(map[EdgeSourceSite][]EdgeIdentity)
	truncated := make(map[EdgeSourceSite]bool)
	for _, state := range states {
		if state.truncated {
			truncated[state.site] = true
			continue
		}
		if len(state.identities) > 0 {
			sortEdgeIdentities(state.identities)
			bySite[state.site] = state.identities
		}
	}
	return bySite, truncated, nil
}
