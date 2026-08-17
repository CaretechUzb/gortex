package graph

import "context"

var (
	_ BoundedOutgoingEdgeIdentityReader = (*OverlaidView)(nil)
	_ BoundedIncomingEdgeIdentityReader = (*OverlaidView)(nil)
)

func overlayAdjacencyCompensatedLimit(layer *OverlayLayer, limit int) int {
	if layer == nil {
		return limit
	}
	// One shadow identity may hide arbitrarily many location-distinct edges.
	// The only honest bounded compensation is the full per-key envelope; the
	// caller-visible limit is re-applied after overlay filtering.
	return MaxBoundedAdjacencyRowsPerKey
}

func (v *OverlaidView) overlayOwnsIdentity(id string) bool {
	return v != nil && v.layer != nil &&
		(v.nodeBelongsToOverlay(id) || v.layer.ownsNodeIdentity(id))
}

func (v *OverlaidView) overlayTargetVisible(id string) bool {
	return !v.overlayOwnsIdentity(id) || v.layer.nodeByID[id] != nil
}

func appendBoundedBaseIdentities(
	ctx context.Context,
	identities []EdgeIdentity,
	limit int,
	budget *boundedAdjacencyBudget,
	accept func(EdgeIdentity) bool,
) ([]EdgeIdentity, bool, error) {
	var out []EdgeIdentity
	seen := make(map[EdgeIdentity]struct{})
	for index, identity := range identities {
		if index&127 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, false, err
			}
		}
		if accept != nil && !accept(identity) {
			continue
		}
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		if err := budget.retain(); err != nil {
			return nil, false, err
		}
		if len(out) == limit {
			return nil, true, nil
		}
		out = append(out, identity)
	}
	sortEdgeIdentities(out)
	return out, false, nil
}

func mergeBoundedIdentitySlices(left, right []EdgeIdentity, limit int) ([]EdgeIdentity, bool) {
	out := make([]EdgeIdentity, 0, len(left)+len(right))
	seen := make(map[EdgeIdentity]struct{}, len(left)+len(right))
	for _, identities := range [][]EdgeIdentity{left, right} {
		for _, identity := range identities {
			if _, duplicate := seen[identity]; duplicate {
				continue
			}
			seen[identity] = struct{}{}
			if len(out) == limit {
				return nil, true
			}
			out = append(out, identity)
		}
	}
	sortEdgeIdentities(out)
	return out, false
}

// FindOutgoingEdgeIdentitiesBounded preserves GetOutEdges replacement and
// tombstone semantics without materializing Edge payloads. Overlay-owned
// sources use only current overlay adjacency; durable rows targeting removed
// overlay identities are filtered before the caller-visible cap.
func (v *OverlaidView) FindOutgoingEdgeIdentitiesBounded(
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
	if v == nil || len(ids) == 0 || len(kindSet) == 0 {
		return projection, nil
	}
	if v.layer == nil {
		if v.base == nil {
			return projection, nil
		}
		bounded, ok := v.base.(BoundedOutgoingEdgeIdentityReader)
		if !ok {
			return BoundedEdgeIdentityProjection{}, ErrBoundedLocalizationUnavailable
		}
		return bounded.FindOutgoingEdgeIdentitiesBounded(ctx, ids, kinds, limit)
	}

	baseIDs := make([]string, 0, len(ids))
	overlayIDs := make(map[string]bool, len(ids))
	for _, id := range ids {
		if v.overlayOwnsIdentity(id) {
			overlayIDs[id] = v.layer.nodeByID[id] != nil
			continue
		}
		baseIDs = append(baseIDs, id)
	}
	baseProjection := emptyBoundedEdgeIdentityProjection()
	if len(baseIDs) > 0 && v.base != nil {
		bounded, ok := v.base.(BoundedOutgoingEdgeIdentityReader)
		if !ok {
			return BoundedEdgeIdentityProjection{}, ErrBoundedLocalizationUnavailable
		}
		baseProjection, err = bounded.FindOutgoingEdgeIdentitiesBounded(
			ctx, baseIDs, kinds, overlayAdjacencyCompensatedLimit(v.layer, limit),
		)
		if err != nil {
			return BoundedEdgeIdentityProjection{}, err
		}
	}

	budget := &boundedAdjacencyBudget{}
	for _, sourceID := range ids {
		if err := ctx.Err(); err != nil {
			return BoundedEdgeIdentityProjection{}, err
		}
		if current, owned := overlayIDs[sourceID]; owned {
			if !current {
				continue
			}
			identities, truncated, scanErr := scanBoundedEdgeIdentities(
				ctx, v.layer.outEdges[sourceID], kindSet, limit, nil, budget,
				func(identity EdgeIdentity) bool { return v.overlayTargetVisible(identity.To) },
			)
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
			continue
		}
		if baseProjection.Truncated[sourceID] {
			projection.Truncated[sourceID] = true
			continue
		}
		identities, truncated, mergeErr := appendBoundedBaseIdentities(
			ctx, baseProjection.ByEndpoint[sourceID], limit, budget,
			func(identity EdgeIdentity) bool {
				return identity.From == sourceID && v.overlayTargetVisible(identity.To) && kindRequested(kindSet, identity.Kind)
			},
		)
		if mergeErr != nil {
			return BoundedEdgeIdentityProjection{}, mergeErr
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

// FindIncomingEdgeIdentitiesBounded preserves GetInEdges semantics: durable
// edges from overlay-owned sources are replaced, then current overlay edges are
// merged. A removed target yields an empty complete key; same-ID replacement
// retains durable callers from untouched sources.
func (v *OverlaidView) FindIncomingEdgeIdentitiesBounded(
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
	if v == nil || len(ids) == 0 || len(kindSet) == 0 {
		return projection, nil
	}
	if v.layer == nil {
		if v.base == nil {
			return projection, nil
		}
		bounded, ok := v.base.(BoundedIncomingEdgeIdentityReader)
		if !ok {
			return BoundedEdgeIdentityProjection{}, ErrBoundedLocalizationUnavailable
		}
		return bounded.FindIncomingEdgeIdentitiesBounded(ctx, ids, kinds, limit)
	}

	baseIDs := make([]string, 0, len(ids))
	removedTargets := make(map[string]bool)
	for _, id := range ids {
		if v.overlayOwnsIdentity(id) && v.layer.nodeByID[id] == nil {
			removedTargets[id] = true
			continue
		}
		baseIDs = append(baseIDs, id)
	}
	baseProjection := emptyBoundedEdgeIdentityProjection()
	if len(baseIDs) > 0 && v.base != nil {
		bounded, ok := v.base.(BoundedIncomingEdgeIdentityReader)
		if !ok {
			return BoundedEdgeIdentityProjection{}, ErrBoundedLocalizationUnavailable
		}
		baseProjection, err = bounded.FindIncomingEdgeIdentitiesBounded(
			ctx, baseIDs, kinds, overlayAdjacencyCompensatedLimit(v.layer, limit),
		)
		if err != nil {
			return BoundedEdgeIdentityProjection{}, err
		}
	}

	budget := &boundedAdjacencyBudget{}
	for _, targetID := range ids {
		if err := ctx.Err(); err != nil {
			return BoundedEdgeIdentityProjection{}, err
		}
		if removedTargets[targetID] {
			continue
		}
		if baseProjection.Truncated[targetID] {
			projection.Truncated[targetID] = true
			continue
		}
		baseIdentities, baseTruncated, mergeErr := appendBoundedBaseIdentities(
			ctx, baseProjection.ByEndpoint[targetID], limit, budget,
			func(identity EdgeIdentity) bool {
				return identity.To == targetID && !v.overlayOwnsIdentity(identity.From) &&
					v.overlayTargetVisible(identity.To) && kindRequested(kindSet, identity.Kind)
			},
		)
		if mergeErr != nil {
			return BoundedEdgeIdentityProjection{}, mergeErr
		}
		if baseTruncated {
			projection.Truncated[targetID] = true
			continue
		}
		overlayIdentities, overlayTruncated, scanErr := scanBoundedEdgeIdentities(
			ctx, v.layer.inEdges[targetID], kindSet, limit, nil, budget,
			func(identity EdgeIdentity) bool {
				return identity.To == targetID && v.overlayTargetVisible(identity.To) &&
					v.overlayOwnsIdentity(identity.From) && v.layer.nodeByID[identity.From] != nil
			},
		)
		if scanErr != nil {
			return BoundedEdgeIdentityProjection{}, scanErr
		}
		if overlayTruncated {
			projection.Truncated[targetID] = true
			continue
		}
		identities, truncated := mergeBoundedIdentitySlices(baseIdentities, overlayIdentities, limit)
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

func kindRequested(kinds map[EdgeKind]struct{}, kind EdgeKind) bool {
	_, ok := kinds[kind]
	return ok
}
