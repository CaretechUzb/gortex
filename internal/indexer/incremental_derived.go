package indexer

import (
	"context"
	"time"

	"go.uber.org/zap"

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
	// ControllerHierarchy counts aspnet:controller stamps propagated down
	// the resolved C# extends chain on declaration-shape changes.
	ControllerHierarchy int
	Framework           int
	ExternalCalls       int
	CrossRepo           int
	Contracts           int
	DurationMs          int64
}

// RunIncrementalDerivedPasses executes only the derived families invalidated
// by the exact per-file plans. A legacy database without persisted fingerprints
// takes the old scoped-global path once; ordinary body/metadata edits never do.
func (mi *MultiIndexer) RunIncrementalDerivedPasses(
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

	if merged.LegacyFallback {
		mi.ArmBatchScope(prefixSet)
		mi.RunGlobalGraphPasses(ctx)
		if merged.Flags.Has(DerivedInvalidatesContracts) {
			report.Contracts = mi.ReconcileContractEdges()
		}
		report.DurationMs = time.Since(started).Milliseconds()
		mi.logIncrementalDerived(report, merged)
		return report
	}
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
	if merged.Flags.Has(DerivedInvalidatesDeclarations) {
		// A save that adds/re-parents a type can hang a new controller off
		// an intermediate base; the pass is one extends-kind scan, cheap
		// enough to re-run whole-graph on declaration-shape changes.
		report.ControllerHierarchy = propagateAspNetControllerHierarchy(mi.graph)
	}
	if merged.Flags.Has(DerivedInvalidatesRuntime) {
		readsEnv, execProc, fields := synthesizeCapabilityEdgesScoped(mi.graph, scopedPrefixes, merged.Files...)
		report.Capability = readsEnv + execProc + fields
	}
	if merged.Flags.Has(DerivedInvalidatesDeclarations) ||
		merged.Flags.Has(DerivedInvalidatesImports) ||
		merged.Flags.Has(DerivedInvalidatesRuntime) {
		framework := resolver.RunFrameworkSynthesizersScopedForFiles(mi.graph, scopedPrefixes, merged.Files)
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
		zap.Int("controller_hierarchy", report.ControllerHierarchy),
		zap.Int("capability_edges", report.Capability),
		zap.Int("framework_edges", report.Framework),
		zap.Int("external_calls", report.ExternalCalls),
		zap.Int("cross_repo_edges", report.CrossRepo),
		zap.Int("contract_edges", report.Contracts),
		zap.Int64("duration_ms", report.DurationMs))
}
