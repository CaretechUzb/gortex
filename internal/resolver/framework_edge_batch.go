package resolver

import (
	"iter"

	"github.com/zzet/gortex/internal/graph"
)

// frameworkEdgeBatchStore turns the legacy synthesizers' AddEdge loops into
// one durable AddBatch at the pass boundary. It is intentionally per-synth:
// flush ordering remains identical to the registry ordering, and a panic in a
// synthesizer discards only that synthesizer's uncommitted additions.
//
// AddEdge captures a deep copy at call time. This preserves the immediate-write
// contract when a caller later reuses or mutates its Edge/Meta value. Logical
// duplicates use last-write-wins, matching Graph.AddEdge and Graph.AddBatch.
type frameworkEdgeBatchStore struct {
	graph.Store
	staged    map[graph.EdgeIdentity]*graph.Edge
	order     []graph.EdgeIdentity
	orderSeen map[graph.EdgeIdentity]struct{}
}

func newFrameworkEdgeBatchStore(store graph.Store) *frameworkEdgeBatchStore {
	return &frameworkEdgeBatchStore{
		Store:     store,
		staged:    make(map[graph.EdgeIdentity]*graph.Edge),
		orderSeen: make(map[graph.EdgeIdentity]struct{}),
	}
}

func (s *frameworkEdgeBatchStore) AllNodes() []*graph.Node {
	panic("framework synthesizer attempted AllNodes")
}

func (s *frameworkEdgeBatchStore) AddEdge(edge *graph.Edge) {
	copy := cloneFrameworkEdge(edge)
	key := graph.EdgeIdentityFor(copy)
	if _, seen := s.orderSeen[key]; !seen {
		s.orderSeen[key] = struct{}{}
		s.order = append(s.order, key)
	}
	s.staged[key] = copy
}

// AddBatch is already set-oriented and may include nodes whose visibility is
// required immediately by the rest of the synthesizer. Forward it unchanged.
// If it supersedes an earlier staged AddEdge at the same identity, the later
// AddBatch wins exactly as it did before this boundary existed.
func (s *frameworkEdgeBatchStore) AddBatch(nodes []*graph.Node, edges []*graph.Edge) {
	for _, edge := range edges {
		if edge != nil {
			delete(s.staged, graph.EdgeIdentityFor(edge))
		}
	}
	s.Store.AddBatch(nodes, edges)
}

// FindEdgesByIdentities preserves the exact-key projection through the staged
// write overlay. A staged edge shadows the durable row with the same identity,
// including when a defensive exact-key check rejects a corrupted staged value.
func (s *frameworkEdgeBatchStore) FindEdgesByIdentities(
	identities []graph.EdgeIdentity,
) map[graph.EdgeIdentity]*graph.Edge {
	out := make(map[graph.EdgeIdentity]*graph.Edge, len(identities))
	seen := make(map[graph.EdgeIdentity]struct{}, len(identities))
	durableKeys := make([]graph.EdgeIdentity, 0, len(identities))
	for _, identity := range identities {
		if _, duplicate := seen[identity]; duplicate {
			continue
		}
		seen[identity] = struct{}{}
		if staged, exists := s.staged[identity]; exists {
			if staged != nil && graph.EdgeIdentityFor(staged) == identity {
				out[identity] = staged
			}
			continue
		}
		durableKeys = append(durableKeys, identity)
	}
	for identity, edge := range findFrameworkEdgesByIdentities(s.Store, durableKeys) {
		out[identity] = edge
	}
	return out
}

// findFrameworkEdgesByIdentities keeps framework adapters exact even when an
// intermediate Store wrapper hides the optional batch finder. The fallback
// performs one batched source-adjacency read and filters the complete logical
// key; it never falls back to AllEdges or one read per identity.
func findFrameworkEdgesByIdentities(
	store graph.Store,
	identities []graph.EdgeIdentity,
) map[graph.EdgeIdentity]*graph.Edge {
	out := make(map[graph.EdgeIdentity]*graph.Edge)
	if len(identities) == 0 {
		return out
	}

	requested := make(map[graph.EdgeIdentity]struct{}, len(identities))
	unique := make([]graph.EdgeIdentity, 0, len(identities))
	for _, identity := range identities {
		if _, duplicate := requested[identity]; duplicate {
			continue
		}
		requested[identity] = struct{}{}
		unique = append(unique, identity)
	}

	if finder, ok := store.(graph.EdgeIdentityBatchFinder); ok {
		for identity, edge := range finder.FindEdgesByIdentities(unique) {
			if _, wanted := requested[identity]; !wanted || edge == nil {
				continue
			}
			if graph.EdgeIdentityFor(edge) == identity {
				out[identity] = edge
			}
		}
		return out
	}

	fromIDs := make([]string, 0, len(unique))
	seenFrom := make(map[string]struct{}, len(unique))
	for _, identity := range unique {
		if identity.From == "" {
			continue
		}
		if _, duplicate := seenFrom[identity.From]; duplicate {
			continue
		}
		seenFrom[identity.From] = struct{}{}
		fromIDs = append(fromIDs, identity.From)
	}
	if len(fromIDs) == 0 {
		return out
	}

	for _, edges := range store.GetOutEdgesByNodeIDs(fromIDs) {
		for _, edge := range edges {
			if edge == nil {
				continue
			}
			identity := graph.EdgeIdentityFor(edge)
			if _, wanted := requested[identity]; wanted {
				out[identity] = edge
			}
		}
	}
	return out
}

func (s *frameworkEdgeBatchStore) flush() {
	if len(s.staged) == 0 {
		return
	}
	edges := make([]*graph.Edge, 0, len(s.staged))
	for _, key := range s.order {
		if edge := s.staged[key]; edge != nil {
			edges = append(edges, edge)
		}
	}
	if len(edges) == 0 {
		return
	}
	// Do not clear before the call. If the backend panics, the panic remains
	// observable and no later synthesizer runs; mutation receipts/errors retain
	// the backend's native AddBatch semantics.
	s.Store.AddBatch(nil, edges)
	s.staged = make(map[graph.EdgeIdentity]*graph.Edge)
	s.order = nil
	s.orderSeen = make(map[graph.EdgeIdentity]struct{})
}

func runLegacyFrameworkSynth(store graph.Store, fn func(graph.Store) int) int {
	batch := newFrameworkEdgeBatchStore(store)
	count := fn(batch)
	batch.flush()
	return count
}

// stagedEdgesMatching returns the edges THIS pass has staged so far, in
// insertion order, filtered by pred — the overlay a fresh kind stream would
// surface for a pass re-reading its own uncommitted additions. Used by the
// shared-stream forms, whose candidate slices predate the pass's own adds.
func (s *frameworkEdgeBatchStore) stagedEdgesMatching(pred func(*graph.Edge) bool) []*graph.Edge {
	var out []*graph.Edge
	for _, key := range s.order {
		if edge := s.staged[key]; edge != nil && pred(edge) {
			out = append(out, edge)
		}
	}
	return out
}

func (s *frameworkEdgeBatchStore) GetOutEdges(id string) []*graph.Edge {
	return s.mergeEdges(s.Store.GetOutEdges(id), func(edge *graph.Edge) bool {
		return edge.From == id
	})
}

func (s *frameworkEdgeBatchStore) GetInEdges(id string) []*graph.Edge {
	return s.mergeEdges(s.Store.GetInEdges(id), func(edge *graph.Edge) bool {
		return edge.To == id
	})
}

func (s *frameworkEdgeBatchStore) GetOutEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	base := s.Store.GetOutEdgesByNodeIDs(ids)
	out := make(map[string][]*graph.Edge, len(ids))
	for _, id := range dedupeFrameworkIDs(ids) {
		out[id] = s.mergeEdges(base[id], func(edge *graph.Edge) bool {
			return edge.From == id
		})
		if len(out[id]) == 0 {
			delete(out, id)
		}
	}
	return out
}

func (s *frameworkEdgeBatchStore) GetInEdgesByNodeIDs(ids []string) map[string][]*graph.Edge {
	base := s.Store.GetInEdgesByNodeIDs(ids)
	out := make(map[string][]*graph.Edge, len(ids))
	for _, id := range dedupeFrameworkIDs(ids) {
		out[id] = s.mergeEdges(base[id], func(edge *graph.Edge) bool {
			return edge.To == id
		})
		if len(out[id]) == 0 {
			delete(out, id)
		}
	}
	return out
}

func (s *frameworkEdgeBatchStore) EdgesByKind(kind graph.EdgeKind) iter.Seq[*graph.Edge] {
	return s.mergeEdgeSeq(s.Store.EdgesByKind(kind), func(edge *graph.Edge) bool {
		return edge.Kind == kind
	})
}

// EdgesByKinds preserves the backend's multi-kind capability through the
// batching facade and overlays staged additions without issuing one point read
// per edge.
func (s *frameworkEdgeBatchStore) EdgesByKinds(kinds []graph.EdgeKind) iter.Seq[*graph.Edge] {
	wanted := make(map[graph.EdgeKind]struct{}, len(kinds))
	unique := make([]graph.EdgeKind, 0, len(kinds))
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, seen := wanted[kind]; seen {
			continue
		}
		wanted[kind] = struct{}{}
		unique = append(unique, kind)
	}
	var base iter.Seq[*graph.Edge]
	if scanner, ok := s.Store.(graph.EdgesByKindsScanner); ok {
		base = scanner.EdgesByKinds(unique)
	} else {
		base = func(yield func(*graph.Edge) bool) {
			for _, kind := range unique {
				for edge := range s.Store.EdgesByKind(kind) {
					if edge != nil && !yield(edge) {
						return
					}
				}
			}
		}
	}
	return s.mergeEdgeSeq(base, func(edge *graph.Edge) bool {
		_, ok := wanted[edge.Kind]
		return ok
	})
}

func (s *frameworkEdgeBatchStore) EdgesLightSeq(kinds ...graph.EdgeKind) iter.Seq[*graph.Edge] {
	wanted := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if kind != "" {
			wanted[kind] = struct{}{}
		}
	}
	return s.mergeEdgeSeq(graph.EdgesLightSeq(s.Store, kinds...), func(edge *graph.Edge) bool {
		_, ok := wanted[edge.Kind]
		return ok
	})
}

func (s *frameworkEdgeBatchStore) EdgesWithUnresolvedTarget() iter.Seq[*graph.Edge] {
	return s.mergeEdgeSeq(s.Store.EdgesWithUnresolvedTarget(), func(edge *graph.Edge) bool {
		return graph.IsUnresolvedTarget(edge.To) && !graph.IsFnValuePlaceholder(edge.To)
	})
}

func (s *frameworkEdgeBatchStore) FnValuePlaceholderEdges() iter.Seq[*graph.Edge] {
	var base iter.Seq[*graph.Edge]
	if scanner, ok := s.Store.(graph.FnValuePlaceholderScanner); ok {
		base = scanner.FnValuePlaceholderEdges()
	} else {
		base = s.Store.EdgesByKind(graph.EdgeReferences)
	}
	return s.mergeEdgeSeq(base, func(edge *graph.Edge) bool {
		return edge.Kind == graph.EdgeReferences && graph.IsFnValuePlaceholder(edge.To)
	})
}

func (s *frameworkEdgeBatchStore) NodesByKinds(kinds []graph.NodeKind) []*graph.Node {
	if scanner, ok := s.Store.(graph.NodesByKindsScanner); ok {
		return scanner.NodesByKinds(kinds)
	}
	wanted := make(map[graph.NodeKind]struct{}, len(kinds))
	seen := make(map[string]struct{})
	var out []*graph.Node
	for _, kind := range kinds {
		if kind == "" {
			continue
		}
		if _, duplicate := wanted[kind]; duplicate {
			continue
		}
		wanted[kind] = struct{}{}
		for node := range s.NodesByKind(kind) {
			if node == nil {
				continue
			}
			if _, duplicate := seen[node.ID]; duplicate {
				continue
			}
			seen[node.ID] = struct{}{}
			out = append(out, node)
		}
	}
	return out
}

func (s *frameworkEdgeBatchStore) ScopeBindingNodesSeq(kinds ...graph.NodeKind) iter.Seq[graph.ScopeBindingNode] {
	return graph.ScopeBindingNodesSeq(s.Store, kinds...)
}

func (s *frameworkEdgeBatchStore) CallableBindingNodesSeq(kinds ...graph.NodeKind) iter.Seq[graph.CallableBindingNode] {
	return graph.CallableBindingNodesSeq(s.Store, kinds...)
}

func (s *frameworkEdgeBatchStore) FileLanguageNodesSeq() iter.Seq[graph.FileLanguageNode] {
	return graph.FileLanguageNodesSeq(s.Store)
}

func (s *frameworkEdgeBatchStore) RepoNodeIdentitiesSeq(repoPrefixes []string, kinds ...graph.NodeKind) iter.Seq[graph.RepoNodeIdentity] {
	return graph.RepoNodeIdentitiesSeq(s.Store, repoPrefixes, kinds...)
}

func (s *frameworkEdgeBatchStore) NamedLanguageNodesSeq(kinds ...graph.NodeKind) iter.Seq[graph.NamedLanguageNode] {
	return graph.NamedLanguageNodesSeq(s.Store, kinds...)
}

func (s *frameworkEdgeBatchStore) QualifiedNodeIdentitiesSeq(kinds ...graph.NodeKind) iter.Seq[graph.QualifiedNodeIdentity] {
	return graph.QualifiedNodeIdentitiesSeq(s.Store, kinds...)
}

func (s *frameworkEdgeBatchStore) FileNodeIdentitiesSeq(repoPrefixes []string) iter.Seq[graph.FileNodeIdentity] {
	return graph.FileNodeIdentitiesSeq(s.Store, repoPrefixes)
}

func (s *frameworkEdgeBatchStore) NodePlacementsByIDs(ids []string) map[string]graph.NodePlacement {
	return graph.NodePlacementsByIDs(s.Store, ids)
}

func (s *frameworkEdgeBatchStore) NodeIDsByKinds(kinds []graph.NodeKind) []string {
	return graph.NodeIDsForKinds(s.Store, kinds...)
}

// ConstantValuesByNodeIDs preserves the Temporal pass's sidecar projection.
// Adapter stores without that optional capability historically expose no
// constant values, represented by an empty successful result.
func (s *frameworkEdgeBatchStore) ConstantValuesByNodeIDs(nodeIDs []string) (map[string]string, error) {
	if reader, ok := s.Store.(graph.ConstantValueReader); ok {
		return reader.ConstantValuesByNodeIDs(nodeIDs)
	}
	return make(map[string]string), nil
}

// MemberMethodsByType preserves the join projection used by the gRPC and C#
// passes. A scoped adapter cannot forward the global backend capability, so its
// fallback performs one predicate scan and one batched node lookup.
func (s *frameworkEdgeBatchStore) MemberMethodsByType() map[string][]graph.MemberMethodInfo {
	stagedMemberOf := false
	for _, edge := range s.staged {
		if edge != nil && edge.Kind == graph.EdgeMemberOf {
			stagedMemberOf = true
			break
		}
	}
	if !stagedMemberOf {
		if reader, ok := s.Store.(graph.MemberMethodsByType); ok {
			return reader.MemberMethodsByType()
		}
	}

	var edges []*graph.Edge
	var methodIDs []string
	for edge := range s.EdgesByKind(graph.EdgeMemberOf) {
		if edge == nil {
			continue
		}
		edges = append(edges, edge)
		methodIDs = append(methodIDs, edge.From)
	}
	methods := s.GetNodesByIDs(dedupeFrameworkIDs(methodIDs))
	out := make(map[string][]graph.MemberMethodInfo)
	for _, edge := range edges {
		method := methods[edge.From]
		if method == nil || method.Kind != graph.KindMethod {
			continue
		}
		out[edge.To] = append(out[edge.To], graph.MemberMethodInfo{
			MethodID:   method.ID,
			Name:       method.Name,
			FilePath:   method.FilePath,
			StartLine:  method.StartLine,
			RepoPrefix: method.RepoPrefix,
		})
	}
	return out
}

func (s *frameworkEdgeBatchStore) GetRepoEdges(repoPrefix string) []*graph.Edge {
	base := s.Store.GetRepoEdges(repoPrefix)
	if len(s.staged) == 0 {
		return base
	}
	fromIDs := make([]string, 0, len(s.staged))
	for _, edge := range s.staged {
		fromIDs = append(fromIDs, edge.From)
	}
	sources := s.GetNodesByIDs(fromIDs)
	return s.mergeEdges(base, func(edge *graph.Edge) bool {
		source := sources[edge.From]
		return source != nil && source.RepoPrefix == repoPrefix
	})
}

func (s *frameworkEdgeBatchStore) RepoEdgesByKinds(
	repoPrefixes []string,
	kinds []graph.EdgeKind,
) []graph.RepoEdgeRow {
	baseRows := graph.ReadRepoEdgesByKinds(s.Store, repoPrefixes, kinds)
	base := make([]*graph.Edge, 0, len(baseRows))
	for _, row := range baseRows {
		if row.Edge != nil {
			base = append(base, row.Edge)
		}
	}
	wantedRepos := frameworkStringSet(repoPrefixes)
	wantedKinds := make(map[graph.EdgeKind]struct{}, len(kinds))
	for _, kind := range kinds {
		wantedKinds[kind] = struct{}{}
	}
	fromIDs := make([]string, 0, len(s.staged))
	for _, edge := range s.staged {
		fromIDs = append(fromIDs, edge.From)
	}
	sources := s.GetNodesByIDs(fromIDs)
	merged := s.mergeEdges(base, func(edge *graph.Edge) bool {
		if _, ok := wantedKinds[edge.Kind]; !ok {
			return false
		}
		source := sources[edge.From]
		if source == nil {
			return false
		}
		_, ok := wantedRepos[source.RepoPrefix]
		return ok
	})
	out := make([]graph.RepoEdgeRow, 0, len(merged))
	for _, edge := range merged {
		source := sources[edge.From]
		if source == nil {
			// A base row already carries a known repository but its source may
			// not be in the staged-source batch. Resolve all such endpoints in
			// one follow-up below rather than issuing a point lookup.
			continue
		}
		out = append(out, graph.RepoEdgeRow{RepoPrefix: source.RepoPrefix, Edge: edge})
	}
	if len(out) != len(merged) {
		allIDs := make([]string, 0, len(merged))
		for _, edge := range merged {
			allIDs = append(allIDs, edge.From)
		}
		allSources := s.GetNodesByIDs(allIDs)
		out = out[:0]
		for _, edge := range merged {
			if source := allSources[edge.From]; source != nil {
				out = append(out, graph.RepoEdgeRow{RepoPrefix: source.RepoPrefix, Edge: edge})
			}
		}
	}
	return out
}

func (s *frameworkEdgeBatchStore) AllEdges() []*graph.Edge {
	panic("framework synthesizer attempted AllEdges")
}

func (s *frameworkEdgeBatchStore) RemoveEdge(from, to string, kind graph.EdgeKind) bool {
	removedStaged := false
	for key, edge := range s.staged {
		if edge != nil && edge.From == from && edge.To == to && edge.Kind == kind {
			delete(s.staged, key)
			removedStaged = true
		}
	}
	return s.Store.RemoveEdge(from, to, kind) || removedStaged
}

func (s *frameworkEdgeBatchStore) mergeEdges(
	base []*graph.Edge,
	include func(*graph.Edge) bool,
) []*graph.Edge {
	if len(s.staged) == 0 {
		return base
	}
	out := make([]*graph.Edge, 0, len(base)+len(s.staged))
	seen := make(map[graph.EdgeIdentity]struct{}, len(s.staged))
	for _, edge := range base {
		if edge == nil {
			continue
		}
		key := graph.EdgeIdentityFor(edge)
		if staged := s.staged[key]; staged != nil {
			seen[key] = struct{}{}
			if include(staged) {
				out = append(out, staged)
			}
			continue
		}
		out = append(out, edge)
	}
	for _, key := range s.order {
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		if edge := s.staged[key]; edge != nil && include(edge) {
			out = append(out, edge)
		}
	}
	return out
}

func (s *frameworkEdgeBatchStore) mergeEdgeSeq(
	base iter.Seq[*graph.Edge],
	include func(*graph.Edge) bool,
) iter.Seq[*graph.Edge] {
	return func(yield func(*graph.Edge) bool) {
		seen := make(map[graph.EdgeIdentity]struct{}, len(s.staged))
		for edge := range base {
			if edge == nil {
				continue
			}
			key := graph.EdgeIdentityFor(edge)
			if staged := s.staged[key]; staged != nil {
				seen[key] = struct{}{}
				if include(staged) && !yield(staged) {
					return
				}
				continue
			}
			if !yield(edge) {
				return
			}
		}
		for _, key := range s.order {
			if _, duplicate := seen[key]; duplicate {
				continue
			}
			if edge := s.staged[key]; edge != nil && include(edge) && !yield(edge) {
				return
			}
		}
	}
}

func dedupeFrameworkIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func cloneFrameworkEdge(edge *graph.Edge) *graph.Edge {
	if edge == nil {
		panic("framework synthesizer attempted to add a nil edge")
	}
	copy := *edge
	copy.Meta = cloneFrameworkMeta(edge.Meta)
	return &copy
}

func cloneFrameworkMeta(meta map[string]any) map[string]any {
	if meta == nil {
		return nil
	}
	copy := make(map[string]any, len(meta))
	for key, value := range meta {
		copy[key] = cloneFrameworkMetaValue(value)
	}
	return copy
}

func cloneFrameworkMetaValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneFrameworkMeta(typed)
	case []string:
		return append([]string(nil), typed...)
	case []any:
		copy := make([]any, len(typed))
		for i, item := range typed {
			copy[i] = cloneFrameworkMetaValue(item)
		}
		return copy
	case map[string]string:
		copy := make(map[string]string, len(typed))
		for key, item := range typed {
			copy[key] = item
		}
		return copy
	default:
		return value
	}
}

var (
	_ graph.Store                     = (*frameworkEdgeBatchStore)(nil)
	_ graph.EdgesByKindsScanner       = (*frameworkEdgeBatchStore)(nil)
	_ graph.NodesByKindsScanner       = (*frameworkEdgeBatchStore)(nil)
	_ graph.FnValuePlaceholderScanner = (*frameworkEdgeBatchStore)(nil)
	_ graph.RepoEdgeKindReader        = (*frameworkEdgeBatchStore)(nil)
	_ graph.ConstantValueReader       = (*frameworkEdgeBatchStore)(nil)
	_ graph.MemberMethodsByType       = (*frameworkEdgeBatchStore)(nil)
	_ graph.EdgeIdentityBatchFinder   = (*frameworkEdgeBatchStore)(nil)
)
