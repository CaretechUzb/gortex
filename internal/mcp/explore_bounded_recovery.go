package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
)

type exploreContextNodesReader interface {
	GetNodesByIDsContext(context.Context, []string) (map[string]*graph.Node, error)
}

func exploreBoundedNodeIDs(ids []string, limit int) ([]string, bool) {
	if limit < 0 {
		return nil, false
	}
	seen := make(map[string]struct{}, min(len(ids), limit))
	out := make([]string, 0, min(len(ids), limit))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		if len(out) >= limit {
			return nil, false
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, true
}

// exploreNodesByIDsBounded exact-refetches only an already-bounded identity
// cohort. SQLite receives the request context; overlay and in-memory readers
// retain their exact-ID batch path with cancellation checked on both sides.
func exploreNodesByIDsBounded(
	ctx context.Context,
	reader graph.Reader,
	ids []string,
	limit int,
) (map[string]*graph.Node, bool) {
	if reader == nil {
		return nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	boundedIDs, ok := exploreBoundedNodeIDs(ids, limit)
	if !ok || ctx.Err() != nil {
		return nil, false
	}
	if len(boundedIDs) == 0 {
		return map[string]*graph.Node{}, true
	}
	var (
		nodes map[string]*graph.Node
		err   error
	)
	if contextual, supported := reader.(exploreContextNodesReader); supported {
		nodes, err = contextual.GetNodesByIDsContext(ctx, boundedIDs)
	} else {
		nodes = reader.GetNodesByIDs(boundedIDs)
	}
	if err != nil || ctx.Err() != nil || len(nodes) != len(boundedIDs) {
		return nil, false
	}
	for _, id := range boundedIDs {
		if nodes[id] == nil {
			return nil, false
		}
	}
	return nodes, true
}

func exploreIncomingSourcesBounded(
	ctx context.Context,
	reader graph.Reader,
	targetIDs []string,
	kind graph.EdgeKind,
	perTargetLimit int,
) (graph.BoundedIncomingSourceProjection, bool) {
	bounded, ok := reader.(graph.BoundedIncomingSourceReader)
	if !ok || perTargetLimit <= 0 {
		return graph.BoundedIncomingSourceProjection{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return graph.BoundedIncomingSourceProjection{}, false
	}
	projection, err := bounded.FindIncomingSourcesBounded(ctx, targetIDs, kind, perTargetLimit)
	if err != nil || ctx.Err() != nil {
		return graph.BoundedIncomingSourceProjection{}, false
	}
	return projection, true
}
