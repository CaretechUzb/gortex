package graph

// ConfigNodeBatchEvicter removes a bounded set of synthetic config-key nodes
// and every incident edge in one backend operation. It is used to retire the
// legacy unqualified Spring IDs during repository-scoped migration.
type ConfigNodeBatchEvicter interface {
	EvictConfigNodesByIDs(ids []string) (nodesRemoved, edgesRemoved int)
}

// EvictConfigNodesByIDs implements the bounded in-memory capability while
// holding all shard locks once. Unknown and non-config-key IDs are ignored.
func (g *Graph) EvictConfigNodesByIDs(ids []string) (nodesRemoved, edgesRemoved int) {
	if g == nil || len(ids) == 0 {
		return 0, 0
	}
	receiptActive := g.beginReceiptMutation()
	if receiptActive {
		defer g.endReceiptMutation()
	}
	g.markMutationReceiptsIncomplete()
	g.lockAllWrite()
	defer g.unlockAllWrite()

	seen := make(map[string]struct{}, len(ids))
	evicted := make(map[string]string)
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		shard := g.shardFor(id)
		node := shard.nodes[id]
		if node == nil || node.Kind != KindConfigKey {
			continue
		}
		evicted[id] = node.RepoPrefix
		shard.repoNodeRemove(node)
		delete(shard.nodes, id)
		if node.QualName != "" {
			if current, ok := shard.byQual[node.QualName]; ok && current.ID == id {
				delete(shard.byQual, node.QualName)
			}
		}
		removeNodeFromBucket(shard.byName, shard.byNameIdx, node.Name, id)
		removeNodeFromBucket(shard.byFile, shard.byFileIdx, node.FilePath, id)
		removeNodeFromBucket(shard.byRepo, shard.byRepoIdx, node.RepoPrefix, id)
	}
	if len(evicted) == 0 {
		return 0, 0
	}
	return len(evicted), g.evictEdgesLocked(evicted)
}

var _ ConfigNodeBatchEvicter = (*Graph)(nil)
