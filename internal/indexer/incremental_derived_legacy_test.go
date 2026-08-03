package indexer

import (
	"context"
	"testing"

	"github.com/zzet/gortex/internal/graph"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestLegacyDerivedPlanKeepsExactFrontier(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	mi := &MultiIndexer{
		graph:  graph.New(),
		logger: zap.New(core),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	report := mi.runIncrementalDerivedPassesTopologyHeld(ctx, map[string]DerivedInvalidationPlan{
		"repo": {
			Flags:          DerivedInvalidatesRuntime,
			Files:          []string{"repo/file.go"},
			LegacyFallback: true,
		},
	})

	if !report.LegacyFallback || report.Repos != 1 || report.Files != 1 {
		t.Fatalf("legacy exact frontier was not preserved: %+v", report)
	}
	if got := logs.FilterMessage("global pass starting").Len(); got != 0 {
		t.Fatalf("legacy exact frontier escalated to %d global pass(es)", got)
	}
}
