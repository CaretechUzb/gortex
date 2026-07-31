package resolver

import (
	"path/filepath"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// prepareIncrementalAttributionCache expands the preloaded changed-file
// frontier with same-package nodes in one multi-path read. The six scoped
// attribution passes reuse this cache instead of repeating file-node queries.
func (r *Resolver) prepareIncrementalAttributionCache(frontier incrementalFileFrontier) {
	r.incrementalNodesByFile = make(map[string][]*graph.Node, len(frontier.nodesByFile))
	for _, path := range frontier.paths {
		// Record empty buckets too; absence would make the helper fall through
		// to a point query for files with no surviving nodes.
		r.incrementalNodesByFile[path] = frontier.nodesByFile[path]
	}
	r.incrementalOutByNode = frontier.outByNode

	missingSet := make(map[string]struct{})
	for _, path := range frontier.paths {
		for _, fileNode := range r.dirIndex[filepath.Dir(path)] {
			if fileNode.FilePath == "" {
				continue
			}
			if _, cached := r.incrementalNodesByFile[fileNode.FilePath]; !cached {
				missingSet[fileNode.FilePath] = struct{}{}
			}
		}
	}
	missing := make([]string, 0, len(missingSet))
	for path := range missingSet {
		missing = append(missing, path)
	}
	if len(missing) > 0 {
		fetched := r.graph.GetFileNodesByPaths(missing)
		for _, path := range missing {
			r.incrementalNodesByFile[path] = fetched[path]
		}
	}
}

// runFileAttributionPassesForFilesLocked preserves the whole-graph pass order
// across a changed-file frontier. Each pass consumes the preloaded node/edge
// cache, and mutations are flushed by pass rather than by file.
func (r *Resolver) runFileAttributionPassesForFilesLocked(frontier incrementalFileFrontier) {
	rebound := false
	if rebinder, ok := r.graph.(graph.GoMethodReceiverBatchRebinder); ok {
		if _, err := rebinder.RebindGoMethodReceiversForFiles(frontier.paths); err == nil {
			rebound = true
		} else {
			r.logger.Warn("resolver: backend batch Go receiver rebind failed; using cached fallback", zap.Error(err))
		}
	}
	if !rebound {
		for _, path := range frontier.paths {
			r.rebindGoMethodReceiversForFile(path)
		}
	}
	for _, path := range frontier.paths {
		r.bindBareNameScopeRefsForFile(path)
	}
	for _, path := range frontier.paths {
		r.bindDataflowCalleeRefsForFile(path)
	}
	for _, path := range frontier.paths {
		r.bindGenericParamRefsForFile(path)
	}
	// Make all scope/dataflow/receiver rewrites visible in backend indexes
	// before the builtin and external materialisation passes inspect them.
	r.flushIncrementalAttributionReindexes()

	if !r.graphHasLanguage("go") {
		return
	}
	var candidates []*graph.Edge
	for _, path := range frontier.paths {
		candidates = append(candidates, r.fileOutEdges(path)...)
	}
	r.attributeGoBuiltinCandidates(candidates)
	seen := make(map[extKey]struct{})
	for _, edge := range candidates {
		collectGoExternalTarget(edge, seen)
	}
	r.materializeGoExternalSeen(seen)
}

func (r *Resolver) flushIncrementalAttributionReindexes() {
	if len(r.incrementalAttributionReindex) > 0 {
		r.graph.ReindexEdges(r.incrementalAttributionReindex)
		r.incrementalAttributionReindex = nil
	}
}

func (r *Resolver) clearIncrementalAttributionCache() {
	r.flushIncrementalAttributionReindexes()
	r.incrementalNodesByFile = nil
	r.incrementalOutByNode = nil
}

func (r *Resolver) persistAttributionReindexes(batch []graph.EdgeReindex) {
	if len(batch) == 0 {
		return
	}
	if r.incrementalNodesByFile != nil {
		r.incrementalAttributionReindex = append(r.incrementalAttributionReindex, batch...)
		return
	}
	r.graph.ReindexEdges(batch)
}

func (r *Resolver) incrementalFileNodes(filePath string) []*graph.Node {
	if r.incrementalNodesByFile != nil {
		if nodes, cached := r.incrementalNodesByFile[filePath]; cached {
			return nodes
		}
	}
	return r.graph.GetFileNodes(filePath)
}
