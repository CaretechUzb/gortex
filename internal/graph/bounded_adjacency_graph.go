package graph

import "context"

// FindOutgoingEdgeIdentitiesBounded projects metadata-free outgoing adjacency.
// Requested kinds are filtered before the per-source limit. Raw adjacency and
// matching identities share request-wide hard budgets; any budget or context
// failure discards the whole projection.
func (g *Graph) FindOutgoingEdgeIdentitiesBounded(
	ctx context.Context,
	sourceIDs []string,
	kinds []EdgeKind,
	limit int,
) (BoundedEdgeIdentityProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	if err := validateBoundedAdjacencyLimit(limit); err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	kindSet, err := canonicalBoundedAdjacencyKinds(ctx, kinds)
	if err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	ids, err := canonicalBoundedAdjacencyEndpoints(ctx, sourceIDs)
	if err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	projection := emptyBoundedEdgeIdentityProjection()
	if g == nil || len(ids) == 0 || len(kindSet) == 0 {
		return projection, nil
	}
	budget := &boundedAdjacencyBudget{}
	for _, sourceID := range ids {
		if err := ctx.Err(); err != nil {
			return BoundedEdgeIdentityProjection{}, err
		}
		shard := g.shardFor(sourceID)
		shard.mu.RLock()
		identities, truncated, scanErr := scanBoundedEdgeIdentities(
			ctx, shard.outEdges[sourceID], kindSet, limit, nil, budget, nil,
		)
		shard.mu.RUnlock()
		if scanErr != nil {
			return BoundedEdgeIdentityProjection{}, scanErr
		}
		if truncated {
			projection.Truncated[sourceID] = true
			continue
		}
		if len(identities) > 0 {
			projection.ByEndpoint[sourceID] = identities
		}
	}
	return projection, nil
}

// FindIncomingEdgeIdentitiesBounded is the inbound sibling, keyed by target.
func (g *Graph) FindIncomingEdgeIdentitiesBounded(
	ctx context.Context,
	targetIDs []string,
	kinds []EdgeKind,
	limit int,
) (BoundedEdgeIdentityProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	if err := validateBoundedAdjacencyLimit(limit); err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	kindSet, err := canonicalBoundedAdjacencyKinds(ctx, kinds)
	if err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	ids, err := canonicalBoundedAdjacencyEndpoints(ctx, targetIDs)
	if err != nil {
		return BoundedEdgeIdentityProjection{}, err
	}
	projection := emptyBoundedEdgeIdentityProjection()
	if g == nil || len(ids) == 0 || len(kindSet) == 0 {
		return projection, nil
	}
	budget := &boundedAdjacencyBudget{}
	for _, targetID := range ids {
		if err := ctx.Err(); err != nil {
			return BoundedEdgeIdentityProjection{}, err
		}
		shard := g.shardFor(targetID)
		shard.mu.RLock()
		identities, truncated, scanErr := scanBoundedEdgeIdentities(
			ctx, shard.inEdges[targetID], kindSet, limit, nil, budget, nil,
		)
		shard.mu.RUnlock()
		if scanErr != nil {
			return BoundedEdgeIdentityProjection{}, scanErr
		}
		if truncated {
			projection.Truncated[targetID] = true
			continue
		}
		if len(identities) > 0 {
			projection.ByEndpoint[targetID] = identities
		}
	}
	return projection, nil
}

// FindOutgoingSiteEdgeIdentitiesBounded projects outgoing identities at exact
// {source,line} sites. Edge FilePath remains output provenance and is never a
// site predicate.
func (g *Graph) FindOutgoingSiteEdgeIdentitiesBounded(
	ctx context.Context,
	sites []EdgeSourceSite,
	kinds []EdgeKind,
	limit int,
) (BoundedSiteEdgeIdentityProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedSiteEdgeIdentityProjection{}, err
	}
	if err := validateBoundedAdjacencyLimit(limit); err != nil {
		return BoundedSiteEdgeIdentityProjection{}, err
	}
	kindSet, err := canonicalBoundedAdjacencyKinds(ctx, kinds)
	if err != nil {
		return BoundedSiteEdgeIdentityProjection{}, err
	}
	canonical, err := canonicalBoundedAdjacencySites(ctx, sites)
	if err != nil {
		return BoundedSiteEdgeIdentityProjection{}, err
	}
	projection := emptyBoundedSiteEdgeIdentityProjection()
	if g == nil || len(canonical) == 0 || len(kindSet) == 0 {
		return projection, nil
	}
	budget := &boundedAdjacencyBudget{}
	for start := 0; start < len(canonical); {
		if err := ctx.Err(); err != nil {
			return BoundedSiteEdgeIdentityProjection{}, err
		}
		end := start + 1
		for end < len(canonical) && canonical[end].From == canonical[start].From {
			end++
		}
		from := canonical[start].From
		shard := g.shardFor(from)
		shard.mu.RLock()
		bySite, truncated, scanErr := scanBoundedSiteEdgeIdentities(
			ctx, shard.outEdges[from], canonical[start:end], kindSet, limit, budget, nil,
		)
		shard.mu.RUnlock()
		if scanErr != nil {
			return BoundedSiteEdgeIdentityProjection{}, scanErr
		}
		for site, identities := range bySite {
			projection.BySite[site] = identities
		}
		for site := range truncated {
			projection.Truncated[site] = true
		}
		start = end
	}
	return projection, nil
}
