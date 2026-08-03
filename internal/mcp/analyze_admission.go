package mcp

import (
	"context"
	"errors"
)

const maxConcurrentExpensiveAnalyses = 1

var (
	errAnalysisBusy        = errors.New("expensive analysis already running; retry after it completes")
	errAnalysisWarmup      = errors.New("expensive analysis unavailable until daemon enrichment completes; retry after workspace readiness reaches enrichment_complete")
	expensiveAnalysisSlots = make(chan struct{}, maxConcurrentExpensiveAnalyses)
)

// expensiveAnalyzeKinds identifies operations that scan or materialize a
// substantial fraction of the graph. They have dedicated admission so editor
// background audits cannot consume every general MCP dispatcher slot.
var expensiveAnalyzeKinds = map[string]struct{}{
	"bottlenecks": {},
	"clusters":    {},
	"components":  {},
	"cycles":      {},
	"dead_code":   {},
	"hotspots":    {},
	"kcore":       {},
	"louvain":     {},
	"pagerank":    {},
	"scc":         {},
	"wcc":         {},
}

func (s *Server) acquireAnalyzeAdmission(ctx context.Context, kind string) (func(), error) {
	if _, expensive := expensiveAnalyzeKinds[kind]; !expensive {
		return func() {}, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s != nil && s.warmupSnapshot().enrichmentIncomplete() {
		return nil, errAnalysisWarmup
	}
	select {
	case expensiveAnalysisSlots <- struct{}{}:
		return func() { <-expensiveAnalysisSlots }, nil
	default:
		return nil, errAnalysisBusy
	}
}
