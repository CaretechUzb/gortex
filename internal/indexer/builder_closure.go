package indexer

import (
	"sort"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// The affected closure of a change.
//
// A derived handle reads only what its own generation carries, with no fall
// through to the layer below. Anything the pass must see has to be in the
// generation, and "what must it see" is answered in both directions around the
// changed files:
//
//   - Backwards, the dependents: a file whose resolved references point INTO a
//     changed file holds edges and reference facts derived against the shape
//     the change is about to alter. It is the same frontier the incremental
//     pipeline re-resolves after a signature change
//     (semanticDependencyFrontierForDeletedFiles walks it for deletions), read
//     here from the base layer before anything is written.
//   - Forwards, the dependencies: a file a changed file resolves INTO holds the
//     definitions the changed file's own references bind to. Without them the
//     pass would re-derive a changed file against an empty world and park every
//     cross-file reference on an unresolved stub — a difference from a whole
//     index of the same tree, not a saving.
//
// Both directions are read twice, from the live adjacency and from the durable
// reference-fact sidecar, and unioned. The sidecar survives evictions the live
// edges do not, and the live edges cover what the sidecar has not been asked to
// record; neither alone is complete.
//
// The walk is ONE hop. A dependent of a dependent is not re-derived, and a
// dependency of a dependency is not carried, so a closure file's own
// cross-file references may bind differently in the generation than they would
// in a whole index of the same tree. Iterating to a fixed point is the whole
// repository in the limit, which is the thing a sparse generation exists to
// avoid; the honest answer is to bound the hop and report the bound, which is
// what ClosureTruncated and the resolution-local producer state do.

// builderClosureCap bounds the closure fan-out. It reuses the incremental
// pipeline's affected-by cap: both answer the same question — how many files
// may one change drag into a re-resolve before the bounded pass stops being
// bounded — and a repository that tuned one has tuned the other.
func (b *SparseGenerationBuilder) builderClosureCap() int {
	if n := b.Config.AffectedByReresolveMax; n > 0 {
		return n
	}
	return defaultAffectedByMax
}

// affectedClosure returns the repo-relative files that must join the changed
// set for the generation to resolve as a whole index of the same tree would.
//
// present is the changed and added set, deleted the removed set; both are
// repo-relative. The result excludes every path in either, so the caller's
// union is a disjoint one. report receives the cap and, when the closure was
// cut, the truncation fact.
func (b *SparseGenerationBuilder) affectedClosure(
	req BuildRequest,
	present, deleted map[string]struct{},
	report *BuildReport,
) []string {
	limit := b.builderClosureCap()
	report.ClosureCap = limit

	seeds := make([]string, 0, len(present)+len(deleted))
	for p := range present {
		seeds = append(seeds, builderGraphPath(req.RepoPrefix, p))
	}
	for p := range deleted {
		seeds = append(seeds, builderGraphPath(req.RepoPrefix, p))
	}
	if len(seeds) == 0 {
		return nil
	}
	sort.Strings(seeds)

	seedSet := make(map[string]struct{}, len(seeds))
	for _, graphPath := range seeds {
		seedSet[graphPath] = struct{}{}
	}
	seedNodeIDs := builderSeedNodeIDs(req.Base, seeds)

	neighbours := make(map[string]struct{})
	b.collectDependents(req, seedNodeIDs, neighbours)
	b.collectDependencies(req, seeds, seedNodeIDs, neighbours)

	closure := make([]string, 0, len(neighbours))
	for graphPath := range neighbours {
		if _, seed := seedSet[graphPath]; seed {
			continue
		}
		rel, owned := builderRelPath(req.RepoPrefix, graphPath)
		if !owned {
			continue
		}
		if _, gone := deleted[rel]; gone {
			continue
		}
		if _, err := req.Target.Stat(rel); err != nil {
			// The base layer knows a file the target state does not hold. The
			// caller's change set did not call it deleted, so this is a diff
			// that does not describe the content — skip it rather than plan a
			// read that would fail.
			continue
		}
		closure = append(closure, rel)
	}
	sort.Strings(closure)
	if len(closure) > limit {
		report.ClosureTruncated = true
		b.Logger.Warn("indexer: sparse generation closure truncated",
			zap.String("repo", req.RepoPrefix),
			zap.Int("closure", len(closure)),
			zap.Int("cap", limit))
		closure = closure[:limit]
	}
	report.ClosureFiles = len(closure)
	report.ClosurePaths = closure
	return closure
}

// builderSeedNodeIDs reads every node the base layer carries at the seed
// paths, in one batched read.
func builderSeedNodeIDs(base graph.Store, seeds []string) []string {
	nodesByFile := base.GetFileNodesByPaths(seeds)
	seen := make(map[string]struct{})
	var ids []string
	for _, graphPath := range seeds {
		for _, node := range nodesByFile[graphPath] {
			if node == nil || node.ID == "" {
				continue
			}
			if _, duplicate := seen[node.ID]; duplicate {
				continue
			}
			seen[node.ID] = struct{}{}
			ids = append(ids, node.ID)
		}
	}
	return ids
}

// collectDependents adds the files whose resolved references point at a seed
// node: one batched reverse-edge read plus the durable reverse lookup.
func (b *SparseGenerationBuilder) collectDependents(
	req BuildRequest,
	seedNodeIDs []string,
	out map[string]struct{},
) {
	if len(seedNodeIDs) == 0 {
		return
	}
	sourceIDs := make(map[string]struct{})
	for _, edges := range req.Base.GetInEdgesByNodeIDs(seedNodeIDs) {
		for _, edge := range edges {
			if edge == nil || !graph.IsResolvableRefEdge(edge.Kind) || graph.IsUnresolvedTarget(edge.From) {
				continue
			}
			sourceIDs[edge.From] = struct{}{}
		}
	}
	builderAddNodeFiles(req.Base, sourceIDs, out)

	if reader, ok := req.Base.(graph.RefFactsReader); ok {
		byFile, err := reader.LoadRefFactsByTargets(req.RepoPrefix, seedNodeIDs)
		if err != nil {
			b.Logger.Debug("indexer: closure reverse fact lookup failed", zap.Error(err))
		}
		for graphPath := range byFile {
			if graphPath != "" {
				out[graphPath] = struct{}{}
			}
		}
	}
}

// collectDependencies adds the files a seed file's resolved references point
// at: one batched forward-edge read plus the durable per-file facts.
func (b *SparseGenerationBuilder) collectDependencies(
	req BuildRequest,
	seeds []string,
	seedNodeIDs []string,
	out map[string]struct{},
) {
	targetIDs := make(map[string]struct{})
	if len(seedNodeIDs) > 0 {
		for _, edges := range req.Base.GetOutEdgesByNodeIDs(seedNodeIDs) {
			for _, edge := range edges {
				if edge == nil || !graph.IsResolvableRefEdge(edge.Kind) || graph.IsUnresolvedTarget(edge.To) {
					continue
				}
				targetIDs[edge.To] = struct{}{}
			}
		}
	}
	if reader, ok := req.Base.(graph.RefFactsReader); ok {
		facts, err := reader.LoadRefFactsByFiles(req.RepoPrefix, seeds)
		if err != nil {
			b.Logger.Debug("indexer: closure forward fact lookup failed", zap.Error(err))
		}
		for _, fact := range facts {
			if fact.ToID != "" && !graph.IsUnresolvedTarget(fact.ToID) {
				targetIDs[fact.ToID] = struct{}{}
			}
		}
	}
	builderAddNodeFiles(req.Base, targetIDs, out)
}

// builderAddNodeFiles resolves node identities to the files they live at, in
// one batched read, and adds those files to out.
//
// The identity's own file component is used as a fallback when the base layer
// has no node under it: an edge may point at a symbol whose definition row was
// evicted, and the ID still names the file the reference was resolved into.
func builderAddNodeFiles(base graph.Store, ids map[string]struct{}, out map[string]struct{}) {
	if len(ids) == 0 {
		return
	}
	list := make([]string, 0, len(ids))
	for id := range ids {
		list = append(list, id)
	}
	sort.Strings(list)
	nodes := base.GetNodesByIDs(list)
	for _, id := range list {
		if node := nodes[id]; node != nil && node.FilePath != "" {
			out[node.FilePath] = struct{}{}
			continue
		}
		if graph.IsStub(id) {
			continue
		}
		if file := graph.IDFile(id); file != "" {
			out[file] = struct{}{}
		}
	}
}
