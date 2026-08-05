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
// background audits cannot consume every general MCP dispatcher slot, and so
// two of them cannot run concurrently and double the peak footprint.
//
// Membership is decided by one question: does the kind materialize the whole
// node or edge corpus? The second group below all do — via scopedNodes (which
// is AllNodes with a Go-side filter) or a direct AllEdges — and were missing,
// so a pair of them could overlap the graph-sized allocation the first group
// is gated against.
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

	// Whole-corpus scanners reached through the same dispatcher.
	"connectivity_health":         {}, // scopedNodes
	"constructors_missing_fields": {}, // scopedNodes
	"doc_staleness":               {}, // AllEdges
	"drupal_hooks":                {}, // AllNodes
	"edge_audit":                  {}, // AllEdges + AllNodes
	"external_calls":              {}, // scopedNodes, three passes
	"role":                        {}, // scopedNodes
	"swiftui_views":               {}, // AllNodes
	"uikit_classes":               {}, // AllNodes
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
