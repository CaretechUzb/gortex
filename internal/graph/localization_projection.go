package graph

import (
	"context"
	"errors"
	"sort"
)

// LocalizationNodeScope is the storage-layer form of a request scope. It is
// intentionally independent of query.QueryOptions so graph does not import the
// query package. Empty fields mean no corresponding restriction.
type LocalizationNodeScope struct {
	WorkspaceID string
	ProjectID   string
	RepoAllow   map[string]bool
	// Kinds is an optional declaration-kind pushdown. It keeps homonym counts
	// meaningful for a caller that can only localize selected node classes.
	Kinds map[NodeKind]bool
	// ExcludeTests preserves localization's production-anchor gate. SQLite
	// evaluates the legacy is_test metadata while keyset-paging bounded rows.
	ExcludeTests bool
	// ExcludeFiles lets an overlay push whole-file tombstones into the base
	// projection before LIMIT. SQLite applies it between bounded keyset pages,
	// avoiding a variable-sized NOT IN clause and SQLite variable limits.
	ExcludeFiles map[string]bool
}

// Allows applies the same effective workspace/project and repository rules as
// query.QueryOptions.ScopeAllows. Keep this predicate in lock-step with that
// method: SQLite projections use the same rules in SQL while the in-memory and
// overlay readers use this implementation.
func (s LocalizationNodeScope) Allows(n *Node) bool {
	if n == nil {
		return false
	}
	if len(s.Kinds) > 0 && !s.Kinds[n.Kind] {
		return false
	}
	if s.ExcludeTests {
		if isTest, _ := n.Meta["is_test"].(bool); isTest {
			return false
		}
	}
	path := n.FilePath
	if path == "" {
		path = IDFile(n.ID)
	}
	if s.ExcludeFiles[path] {
		return false
	}
	if s.WorkspaceID != "" {
		workspace := n.WorkspaceID
		if workspace == "" {
			workspace = n.RepoPrefix
		}
		if workspace != s.WorkspaceID {
			return false
		}
		if s.ProjectID != "" {
			project := n.ProjectID
			if project == "" {
				project = n.RepoPrefix
			}
			if project != s.ProjectID {
				return false
			}
		}
	}
	// Empty RepoPrefix nodes are global synthetic externals and remain visible
	// under a repository narrow, matching QueryOptions.ScopeAllows.
	return len(s.RepoAllow) == 0 || n.RepoPrefix == "" || s.RepoAllow[n.RepoPrefix]
}

// BoundedNodeProjection is a deterministic, request-bounded node page. Total
// is the number of admitted rows observed up to limit+1, not a corpus-wide
// count. Truncated means that sentinel was reached. This saturation contract is
// sufficient for ambiguity decisions while bounding both memory and row work.
type BoundedNodeProjection struct {
	Nodes     []*Node
	Total     int
	Truncated bool
}

// BoundedExactNameReader is the localization exact-name projection. It is an
// optional capability instead of part of Reader so callers cannot silently
// fall back to the legacy unbounded, full-row FindNodesByName path.
type BoundedExactNameReader interface {
	FindNodesByNameBounded(context.Context, string, LocalizationNodeScope, int) (BoundedNodeProjection, error)
}

var _ BoundedExactNameReader = (*Graph)(nil)

// FindNodesByNameBounded reads at most limit+1 matching pointers while still
// counting every scoped homonym. The bounded sorted insertion keeps memory
// proportional to the response cap even for names shared by tens of thousands
// of declarations. This legacy in-memory backend takes one read lock per shard,
// so concurrent mutation may produce a weak cross-shard snapshot; each returned
// page still obeys its cap and Total is never smaller than len(Nodes). SQLite,
// the release backend, provides a single read-transaction snapshot.
func (g *Graph) FindNodesByNameBounded(
	ctx context.Context,
	name string,
	scope LocalizationNodeScope,
	limit int,
) (BoundedNodeProjection, error) {
	if g == nil || name == "" || limit <= 0 {
		return BoundedNodeProjection{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedNodeProjection{}, err
	}

	pageSize := limit + 1
	kept := make([]*Node, 0, pageSize)
	total := 0
	for _, shard := range g.shards {
		if err := ctx.Err(); err != nil {
			return BoundedNodeProjection{}, err
		}
		shard.mu.RLock()
		for i, node := range shard.byName[name] {
			if i&127 == 0 {
				if err := ctx.Err(); err != nil {
					shard.mu.RUnlock()
					return BoundedNodeProjection{}, err
				}
			}
			if !scope.Allows(node) {
				continue
			}
			total++
			kept = insertBoundedLocalizationNode(kept, node, pageSize)
		}
		shard.mu.RUnlock()
	}

	truncated := len(kept) > limit || total > limit
	if len(kept) > limit {
		kept = kept[:limit]
	}
	if total > pageSize {
		total = pageSize
	}
	return BoundedNodeProjection{Nodes: kept, Total: total, Truncated: truncated}, nil
}

func insertBoundedLocalizationNode(nodes []*Node, node *Node, limit int) []*Node {
	if node == nil || limit <= 0 {
		return nodes
	}
	position := sort.Search(len(nodes), func(i int) bool { return nodes[i].ID >= node.ID })
	if position < len(nodes) && nodes[position].ID == node.ID {
		return nodes
	}
	if position >= limit {
		return nodes
	}
	nodes = append(nodes, nil)
	copy(nodes[position+1:], nodes[position:])
	nodes[position] = node
	if len(nodes) > limit {
		nodes = nodes[:limit]
	}
	return nodes
}

var (
	_ BoundedExactNameReader = (*OverlaidView)(nil)
	// ErrBoundedLocalizationUnavailable lets MCP localization fail closed when
	// a third-party Reader has not implemented the bounded projection. Falling
	// back to FindNodesByName would silently restore the unbounded allocation.
	ErrBoundedLocalizationUnavailable = errors.New("bounded localization projection unavailable")
)

// FindNodesByNameBounded merges a bounded base page with the request overlay.
// It inflates the base cap only by the exact set of base identities the layer
// can shadow, so filtering a replacement/tombstone cannot leave a short page.
func (v *OverlaidView) FindNodesByNameBounded(
	ctx context.Context,
	name string,
	scope LocalizationNodeScope,
	limit int,
) (BoundedNodeProjection, error) {
	if v == nil || name == "" || limit <= 0 {
		return BoundedNodeProjection{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BoundedNodeProjection{}, err
	}

	pageSize := limit + 1
	kept := make([]*Node, 0, pageSize)
	overlayCount := 0
	shadowIDs := make(map[string]struct{})
	if v.layer != nil {
		for id := range v.layer.nameRemoved[name] {
			shadowIDs[id] = struct{}{}
		}
		for _, node := range v.layer.nodesByName[name] {
			if node == nil {
				continue
			}
			shadowIDs[node.ID] = struct{}{}
			if !scope.Allows(node) {
				continue
			}
			overlayCount++
			kept = insertBoundedLocalizationNode(kept, node, pageSize)
		}
	}

	if v.base == nil {
		if len(kept) > limit {
			kept = kept[:limit]
		}
		return BoundedNodeProjection{
			Nodes: kept, Total: overlayCount, Truncated: overlayCount > limit,
		}, nil
	}
	baseReader, ok := v.base.(BoundedExactNameReader)
	if !ok {
		return BoundedNodeProjection{}, ErrBoundedLocalizationUnavailable
	}

	// Push whole-file replacement/tombstone filtering into the base projection.
	// A MarkFile-only layer can hide arbitrarily many leading homonyms without
	// populating nameRemoved; filtering after LIMIT would omit later visible
	// rows. Clone the caller map so overlay state never mutates request scope.
	baseScope := scope
	if v.layer != nil && len(v.layer.entries) > 0 {
		baseScope.ExcludeFiles = make(map[string]bool, len(scope.ExcludeFiles)+len(v.layer.entries))
		for path, excluded := range scope.ExcludeFiles {
			if excluded {
				baseScope.ExcludeFiles[path] = true
			}
		}
		for path := range v.layer.entries {
			baseScope.ExcludeFiles[path] = true
		}
	}

	// Inflate only by detached identities this exact-name bucket can shadow.
	// Whole-file shadows were already removed by baseScope.ExcludeFiles, so
	// counting their identities again would let one large overlay defeat the
	// projection's hard row/allocation bound.
	detachedShadows := 0
	for id := range shadowIDs {
		path := IDFile(id)
		if path == "" || !baseScope.ExcludeFiles[path] {
			detachedShadows++
		}
	}
	baseLimit := limit + detachedShadows
	basePage, err := baseReader.FindNodesByNameBounded(ctx, name, baseScope, baseLimit)
	if err != nil {
		return BoundedNodeProjection{}, err
	}
	visible := overlayCount
	for _, node := range basePage.Nodes {
		if node == nil {
			continue
		}
		if _, shadowed := shadowIDs[node.ID]; shadowed {
			continue
		}
		if v.layer != nil && v.layer.HasFile(IDFile(node.ID)) {
			continue
		}
		visible++
		kept = insertBoundedLocalizationNode(kept, node, pageSize)
	}
	// Saturation of the shadow-inflated base page proves at least limit+1
	// visible rows: no more than detachedShadows returned identities can vanish.
	if basePage.Truncated && visible <= limit {
		visible = limit + 1
	}
	if visible > limit+1 {
		visible = limit + 1
	}
	truncated := visible > limit
	if len(kept) > limit {
		kept = kept[:limit]
	}
	return BoundedNodeProjection{Nodes: kept, Total: visible, Truncated: truncated}, nil
}
