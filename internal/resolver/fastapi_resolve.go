package resolver

import (
	"iter"
	"strings"

	"github.com/zzet/gortex/internal/graph"
)

// FastAPI dependency / router directory-convention fallback. The Python
// extractor stamps a `Depends(get_db)` argument as a `via=fastapi.Depends`
// call placeholder and an `include_router(api_router)` argument as a
// `via=fastapi.router` reference placeholder. When the standard import/
// reference resolver already bound the target (the precise path) those
// placeholders are no longer unresolved and this pass leaves them alone. Only
// the residual unresolved ones are bound by directory convention —
// dependencies under /dependencies/ /deps/ /core/, routers under /routers/
// /api/ /routes/ /endpoints/ — so recall improves without regressing the
// precise path and without double-binding.

var (
	fastapiDepDirs    = []string{"/dependencies/", "/deps/", "/core/"}
	fastapiRouterDirs = []string{"/routers/", "/api/", "/routes/", "/endpoints/"}
)

// ResolveFastAPIDeps binds residual unresolved FastAPI dependency / router
// references to their definitions by directory convention. Returns the count
// bound.
func ResolveFastAPIDeps(g graph.Store) int {
	return resolveFastAPIDeps(g, nil)
}

// resolveFastAPIDeps is the census-multiplexed form: cands, when non-nil,
// replaces both whole-stream decodes with the shared walk's pre-matched
// candidates.
func resolveFastAPIDeps(g graph.Store, cands *frameworkPassCandidates) int {
	if g == nil {
		return 0
	}
	callsStream := g.EdgesByKind(graph.EdgeCalls)
	refsStream := g.EdgesByKind(graph.EdgeReferences)
	if cands != nil {
		callsStream = frameworkEdgeSeq(cands.calls)
		refsStream = frameworkEdgeSeq(cands.refs)
	}
	resolved := 0
	var reindex []graph.EdgeReindex
	for _, stream := range []iter.Seq[*graph.Edge]{callsStream, refsStream} {
		for e := range stream {
			if e == nil || e.Meta == nil || !graph.IsUnresolvedTarget(e.To) {
				continue
			}
			var preferDirs []string
			switch via, _ := e.Meta["via"].(string); via {
			case "fastapi.Depends":
				preferDirs = fastapiDepDirs
			case "fastapi.router":
				preferDirs = fastapiRouterDirs
			default:
				continue
			}
			name := graph.UnresolvedName(e.To)
			if strings.ContainsRune(name, '.') {
				continue // member-expr target — left to the import resolver
			}
			fromFile := ""
			if n := g.GetNode(e.From); n != nil {
				fromFile = n.FilePath
			}
			if !strings.HasSuffix(fromFile, ".py") {
				continue
			}
			targetID, conf := ResolveByConvention(g, name, "", preferDirs, fromFile)
			if targetID == "" {
				continue
			}
			oldTo := e.To
			e.To = targetID
			e.Origin = graph.OriginASTInferred
			e.Confidence = conf
			e.ConfidenceLabel = graph.ConfidenceLabelFor(e.Kind, conf)
			StampSynthesized(e, SynthFastAPIResolve)
			reindex = append(reindex, graph.EdgeReindex{Edge: e, OldTo: oldTo})
			resolved++
		}
	}
	if len(reindex) > 0 {
		g.ReindexEdges(reindex)
	}
	return resolved
}
