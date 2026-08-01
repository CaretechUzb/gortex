package indexer

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/entrypoints"
	"github.com/zzet/gortex/internal/reach"
	"github.com/zzet/gortex/internal/resolver"
)

// IncrementalDerivedReport exposes the work selected by the changed-file
// invalidation plans. It is intentionally count-only so telemetry remains
// bounded even for a large workspace.
type IncrementalDerivedReport struct {
	Repos          int
	Files          int
	TypeIDs        int
	LegacyFallback bool
	Implements     int
	Overrides      int
	TestSymbols    int
	TestEdges      int
	Capability     int
	// EntryPointHierarchy counts entry-point stamps (aspnet:controller
	// today) propagated down resolved extends chains on declaration-shape
	// changes.
	EntryPointHierarchy int
	Framework           int
	ExternalCalls       int
	CrossRepo           int
	Contracts           int
	DurationMs          int64
}

// runStandaloneIncrementalDerivedPasses reuses the exact MultiIndexer derived
// coordinator for a direct Indexer. The synthetic coordinator intentionally
// contains only this repository; real shared-graph callers must use their owning
// MultiIndexer so contract and cross-repository passes see every sibling.
func (idx *Indexer) runStandaloneIncrementalDerivedPasses(plan DerivedInvalidationPlan) IncrementalDerivedReport {
	if idx == nil || idx.graph == nil || idx.deferGlobalPasses.Load() || plan.Empty() {
		return IncrementalDerivedReport{}
	}
	logger := idx.logger
	if logger == nil {
		logger = zap.NewNop()
	}
	prefix := idx.RepoPrefix()
	mi := NewMultiIndexer(idx.graph, idx.registry, idx.search, nil, logger)
	mi.indexers[prefix] = idx
	mi.repos[prefix] = &RepoMetadata{
		RepoPrefix: prefix,
		RootPath:   idx.RootPath(),
	}
	return mi.runIncrementalDerivedPassesTopologyHeld(context.Background(), map[string]DerivedInvalidationPlan{
		prefix: plan,
	})
}

// RunIncrementalDerivedPasses executes only the derived families invalidated
// by the exact per-file plans. A legacy database without persisted fingerprints
// takes the old scoped-global path once; ordinary body/metadata edits never do.
func (mi *MultiIndexer) RunIncrementalDerivedPasses(
	ctx context.Context,
	plans map[string]DerivedInvalidationPlan,
) IncrementalDerivedReport {
	if mi == nil || mi.graph == nil || len(plans) == 0 {
		return IncrementalDerivedReport{}
	}
	if ctx != nil {
		select {
		case <-ctx.Done():
			return IncrementalDerivedReport{}
		default:
		}
	}
	hasWork := false
	for _, plan := range plans {
		if !plan.Empty() {
			hasWork = true
			break
		}
	}
	if !hasWork {
		return IncrementalDerivedReport{}
	}
	mi.batchMutationGate.Lock()
	defer mi.batchMutationGate.Unlock()
	if ctx != nil {
		select {
		case <-ctx.Done():
			return IncrementalDerivedReport{}
		default:
		}
	}
	finishTopologyMutation := reach.BeginTopologyMutation(mi.graph)
	defer finishTopologyMutation(true)
	return mi.runIncrementalDerivedPassesTopologyHeld(ctx, plans)
}

// runIncrementalDerivedPassesTopologyHeld is the non-reentrant implementation
// for mutation pipelines that already own the global topology writer.
func (mi *MultiIndexer) runIncrementalDerivedPassesTopologyHeld(
	ctx context.Context,
	plans map[string]DerivedInvalidationPlan,
) IncrementalDerivedReport {
	started := time.Now()
	report := IncrementalDerivedReport{}
	if mi == nil || mi.graph == nil || len(plans) == 0 {
		return report
	}

	var merged DerivedInvalidationPlan
	prefixSet := make(map[string]struct{}, len(plans))
	prefixScope := make(map[string]bool, len(plans))
	for prefix, plan := range plans {
		if plan.Empty() {
			continue
		}
		merged.Merge(plan)
		prefixSet[prefix] = struct{}{}
		prefixScope[prefix] = true
	}
	if len(prefixSet) == 0 {
		return report
	}
	report.Repos = len(prefixSet)
	report.Files = len(merged.Files)
	report.TypeIDs = len(merged.TypeIDs)
	report.LegacyFallback = merged.LegacyFallback

	// A single unprefixed repo cannot use repo-scoped readers. Falling back to
	// nil here preserves the old whole-graph behavior, which is still bounded
	// to that one repository.
	scopedPrefixes := prefixScope
	if prefixScope[""] {
		scopedPrefixes = nil
	}

	// A pre-fingerprint row cannot classify which derived family changed, so
	// its producer conservatively sets every relevant flag. The file/type/
	// contract frontiers are still exact: escalating that first edit to a
	// repository-wide global pass made an upgraded warm daemon spend minutes
	// rebuilding unrelated framework edges. Run the exact coordinator below
	// with the conservative flags and retain LegacyFallback as telemetry.
	if ctx != nil {
		select {
		case <-ctx.Done():
			report.DurationMs = time.Since(started).Milliseconds()
			return report
		default:
		}
	}

	// Batched IndexFile calls deliberately do not update the clone index because
	// its old function signatures are gone by the time this coordinator runs.
	// Mark incompleteness rather than rebuilding a whole repo on an edit; the
	// explicit clone/global consumer owns the eventual bounded rebuild.
	mi.mu.RLock()
	for prefix := range prefixSet {
		if idx := mi.indexers[prefix]; idx != nil && idx.cloneIndex != nil {
			idx.cloneIndex.MarkPending()
		}
	}
	mi.mu.RUnlock()

	if merged.Flags.Has(DerivedInvalidatesDeclarations) && len(merged.TypeIDs) > 0 {
		typeFrontier := make(map[string]bool, len(merged.TypeIDs))
		for _, id := range merged.TypeIDs {
			typeFrontier[id] = true
		}
		r := resolver.New(mi.graph)
		report.Implements = r.InferImplementsScoped(typeFrontier, typeFrontier)
		report.Overrides = r.InferOverridesScoped(typeFrontier)
	}

	if merged.Flags.Has(DerivedInvalidatesRuntime) || merged.Flags.Has(DerivedInvalidatesTests) {
		report.TestSymbols, report.TestEdges = markTestSymbolsAndEmitEdgesScoped(mi.graph, scopedPrefixes, merged.Files...)
	}
	if merged.Flags.Has(DerivedInvalidatesDeclarations) && len(merged.TypeIDs) > 0 {
		// A save that adds/re-parents a type can hang a new derived type
		// off a stamped hierarchy (or stamp a new base); the changed-type
		// frontier drives seed discovery, like the scoped passes above.
		report.EntryPointHierarchy = entrypoints.PropagateEntryPointsDownHierarchyScoped(mi.graph, merged.TypeIDs)
	}
	if merged.Flags.Has(DerivedInvalidatesRuntime) {
		readsEnv, execProc, fields := synthesizeCapabilityEdgesScoped(mi.graph, scopedPrefixes, merged.Files...)
		report.Capability = readsEnv + execProc + fields
	}
	if merged.Flags.Has(DerivedInvalidatesDeclarations) ||
		merged.Flags.Has(DerivedInvalidatesImports) ||
		merged.Flags.Has(DerivedInvalidatesRuntime) {
		framework := resolver.RunFrameworkSynthesizersScopedForFiles(
			mi.graph, scopedPrefixes, merged.Files, merged.CSharpHierarchyChanged,
		)
		report.Framework = framework.Total
		report.ExternalCalls = resolver.SynthesizeExternalCallsForFiles(
			mi.graph, mi.externalCallSynthesisEnabled(), merged.Files,
		)
		report.CrossRepo = resolver.DetectCrossRepoEdgesForFiles(mi.graph, merged.Files)
	}
	if merged.Flags.Has(DerivedInvalidatesContracts) {
		report.Contracts = mi.ReconcileContractEdgesForFrontier(merged)
	}

	report.DurationMs = time.Since(started).Milliseconds()
	mi.logIncrementalDerived(report, merged)
	return report
}

func (mi *MultiIndexer) logIncrementalDerived(report IncrementalDerivedReport, plan DerivedInvalidationPlan) {
	mi.logger.Info("incremental derived passes complete",
		zap.Int("repos", report.Repos),
		zap.Int("files", report.Files),
		zap.Int("type_ids", report.TypeIDs),
		zap.Uint32("flags", uint32(plan.Flags)),
		zap.Bool("legacy_fallback", report.LegacyFallback),
		zap.Int("implements", report.Implements),
		zap.Int("overrides", report.Overrides),
		zap.Int("test_edges", report.TestEdges),
		zap.Int("entrypoint_hierarchy", report.EntryPointHierarchy),
		zap.Int("capability_edges", report.Capability),
		zap.Int("framework_edges", report.Framework),
		zap.Int("external_calls", report.ExternalCalls),
		zap.Int("cross_repo_edges", report.CrossRepo),
		zap.Int("contract_edges", report.Contracts),
		zap.Int64("duration_ms", report.DurationMs))
}
