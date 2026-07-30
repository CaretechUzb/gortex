package indexer

import (
	"fmt"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// watcherBatchReindex is the common large-change entry point used by both the
// filesystem storm drainer and the git ref reconciler. Production MultiWatcher
// instances replace the per-indexer fallback with MultiIndexer so the exact
// mutation frontier also receives the shared resolver and derived catch-up.
type watcherBatchReindex func(paths []string) (*IndexResult, error)

// incrementalReindexPathsWithReceipt keeps the mutation receipt boundary tight:
// only the bounded parse/evict pipeline is observed. Resolution and derived
// mutations happen after the receipt has closed and therefore cannot widen the
// next incremental frontier.
func (idx *Indexer) incrementalReindexPathsWithReceipt(
	root string,
	paths []string,
) (result *IndexResult, receipt *graph.MutationReceipt, batch *reparsePendingEnrichmentBatch, err error) {
	batch = &reparsePendingEnrichmentBatch{deferResolverCatchup: true}
	receiptStore, _ := idx.graph.(graph.MutationReceiptStore)
	if receiptStore == nil {
		result, err = idx.incrementalReindexPaths(root, paths, true, batch)
		return result, nil, batch, err
	}

	token := receiptStore.BeginMutationReceipt()
	defer func() {
		observed := receiptStore.EndMutationReceipt(token)
		receipt = &observed
	}()
	result, err = idx.incrementalReindexPaths(root, paths, true, batch)
	return result, receipt, batch, err
}

// incrementalResolutionFrontier chooses the narrowest quality-safe resolver
// scope. A complete receipt is authoritative. Stores without receipt support
// can still use the successful-file DerivedInvalidationPlan emitted by the
// bounded pipeline. An incomplete receipt takes that conservative scoped
// fallback once; only a complete irrelevant receipt can prove no catch-up work.
func incrementalResolutionFrontier(
	result *IndexResult,
	receipt *graph.MutationReceipt,
) (files []string, needed bool, exact bool) {
	if result == nil {
		return nil, false, true
	}
	if receipt != nil {
		if !receipt.Complete {
			files = appendUniqueSorted(nil, result.DerivedInvalidation.Files...)
			return files, true, false
		}
		if !receipt.ResolutionRelevant {
			return nil, false, true
		}
		files = receipt.ResolutionFiles()
		if len(files) == 0 {
			files = appendUniqueSorted(nil, result.DerivedInvalidation.Files...)
			return files, true, false
		}
		return files, true, true
	}

	files = append([]string(nil), result.DerivedInvalidation.Files...)
	if len(files) == 0 {
		if result.StaleFileCount > 0 || result.DeletedFileCount > 0 {
			return nil, true, false
		}
		return nil, false, true
	}
	sort.Strings(files)
	files = compactSortedStrings(files)
	return files, true, true
}

func compactSortedStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	write := 1
	for read := 1; read < len(values); read++ {
		if values[read] == values[write-1] {
			continue
		}
		values[write] = values[read]
		write++
	}
	return values[:write]
}

func (idx *Indexer) observeIncrementalCatchup(kind string, files []string) {
	if idx == nil || idx.incrementalCatchupHook == nil {
		return
	}
	idx.incrementalCatchupHook(kind, append([]string(nil), files...))
}

// runIncrementalResolutionCatchup owns every resolution-dependent tail for a
// deferred incremental mutation. The changed-file resolver, dataflow rewrite,
// affected-by resolver and durable fact refresh each run at most once for the
// complete batch.
func (idx *Indexer) runIncrementalResolutionCatchup(
	files []string,
	batch *reparsePendingEnrichmentBatch,
	resolveFiles func([]string),
) {
	files = appendUniqueSorted(nil, files...)
	if len(files) == 0 {
		return
	}
	idx.observeIncrementalCatchup("resolve", files)
	resolveFiles(files)

	idx.observeIncrementalCatchup("dataflow", files)
	idx.materializeDataflowParamsForFiles(files)

	affected := batch.deferredAffectedPlan()
	idx.executeAffectedByPlan(affected)

	// Facts are rebuilt once, after both resolver frontiers are current. This
	// avoids one delete/set transaction per structural chunk and also ensures an
	// affected caller that degraded to a stub drops its stale durable fact.
	factFiles := appendUniqueSorted(files, affected.files...)
	idx.observeIncrementalCatchup("ref_facts", factFiles)
	idx.persistRefFactsForFiles(factFiles)
}

func (idx *Indexer) runIncrementalWatcherSemantic(graphPaths []string) {
	if idx == nil || idx.semanticMgr == nil || !idx.semanticMgr.Enabled() ||
		!idx.semanticMgr.HasProviders() || !idx.semanticMgr.EnrichesOnWatch() {
		return
	}
	pendingFiles, _, _ := idx.deferredEnrichScope()
	if len(pendingFiles) == 0 {
		pendingFiles = appendUniqueSorted(nil, graphPaths...)
	}
	idx.observeIncrementalCatchup("semantic", pendingFiles)
	idx.runDeferredEnrich()
	if idx.pendingEnrich.Load() {
		return
	}
	pending := make(map[string]bool, len(graphPaths))
	for _, graphPath := range graphPaths {
		if graphPath != "" {
			pending[graphPath] = false
		}
	}
	idx.setReparsePendingEnrichments(pending)
}

// incrementalReindexWatcherPaths is the complete direct-Indexer coordinator used
// outside a MultiIndexer. It performs one bounded parse/evict batch, one exact
// resolver/reference-fact catch-up, semantic refresh, and the precise standalone
// derived passes. Shared-graph callers use MultiIndexer so cross-repository work
// sees every sibling registry and indexer.
func (idx *Indexer) incrementalReindexWatcherPaths(root string, paths []string) (*IndexResult, error) {
	result, receipt, batch, err := idx.incrementalReindexPathsWithReceipt(root, paths)
	if err != nil {
		return result, err
	}
	files, needed, exact := incrementalResolutionFrontier(result, receipt)
	if needed && len(files) > 0 {
		idx.runIncrementalResolutionCatchup(files, batch, func(frontier []string) {
			if idx.incrementalResolveFilesHook != nil {
				idx.incrementalResolveFilesHook(frontier)
				return
			}
			idx.resolver.ResolveFilesAndIncoming(frontier)
		})
	} else if needed && !exact {
		idx.observeIncrementalCatchup("resolve", nil)
		idx.ResolveAll()
	}
	if !idx.deferGlobalPasses {
		idx.runIncrementalWatcherSemantic(result.DerivedInvalidation.Files)
		idx.observeIncrementalCatchup("derived", result.DerivedInvalidation.Files)
		idx.runStandaloneIncrementalDerivedPasses(result.DerivedInvalidation)
	}
	return result, nil
}

func (w *Watcher) reindexStormPaths(paths []string) (*IndexResult, error) {
	if w.batchReindex != nil {
		return w.batchReindex(paths)
	}
	if w.indexer == nil || w.indexer.rootPath == "" {
		return nil, fmt.Errorf("watcher: index root is not initialized")
	}
	return w.indexer.incrementalReindexWatcherPaths(w.indexer.rootPath, paths)
}

func (gw *GitWatcher) reindexChangedPaths(paths []string) (*IndexResult, error) {
	if gw.batchReindex != nil {
		return gw.batchReindex(paths)
	}
	if gw.indexer == nil {
		return nil, fmt.Errorf("git-watcher: indexer is not initialized")
	}
	return gw.indexer.incrementalReindexWatcherPaths(gw.repoPath, paths)
}

// resolveIncrementalRepoMutation runs one shared resolver pass for a complete
// receipt frontier or, when the store cannot certify its eviction shape, the
// indexer's conservative successful-file frontier. A whole-graph fallback is
// reserved for the exceptional case where neither source yields any scope.
func (mi *MultiIndexer) resolveIncrementalRepoMutation(
	repoPrefix string,
	result *IndexResult,
	receipt *graph.MutationReceipt,
	batch *reparsePendingEnrichmentBatch,
) {
	files, needed, _ := incrementalResolutionFrontier(result, receipt)
	idx := mi.GetIndexer(repoPrefix)
	if needed && len(files) > 0 && idx != nil {
		idx.runIncrementalResolutionCatchup(files, batch, func(frontier []string) {
			if idx.incrementalResolveFilesHook != nil {
				idx.incrementalResolveFilesHook(frontier)
				return
			}
			mi.runMasterResolveFiles(frontier, false)
		})
	} else if needed && len(files) > 0 {
		mi.runMasterResolveFiles(files, false)
	} else if needed {
		scope := map[string]struct{}{repoPrefix: {}}
		if repoPrefix == "" {
			scope = nil
		}
		mi.runMasterResolve(scope, false)
		// No scoped frontier survived. Refresh any known files without adding a
		// second graph-wide dataflow or durable-fact scan.
		if idx != nil && result != nil {
			known := appendUniqueSorted(nil, result.DerivedInvalidation.Files...)
			idx.observeIncrementalCatchup("dataflow", known)
			idx.materializeDataflowParamsForFiles(known)
			idx.observeIncrementalCatchup("ref_facts", known)
			idx.persistRefFactsForFiles(known)
		}
	}
	if idx != nil && result != nil {
		idx.runIncrementalWatcherSemantic(result.DerivedInvalidation.Files)
	}
}
