package indexer

import "github.com/zzet/gortex/internal/graph"

func (idx *Indexer) newStructuralIntegrityShadow(owner graph.Store, path graph.StructuralDropPath) *graph.Graph {
	return graph.NewStructuralIntegrityShadow(owner, idx.RepoPrefix(), path)
}
