package mcp

import (
	"context"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

// Leading-file depth allocation.
//
// Per-file diversification demotes a file's hits once it reaches the per-file
// cap, so a bounded localization page spends nearly every slot on one row per
// file. That is the right trade while ranking is still undecided, and the wrong
// one once it has settled on a file: the sibling declarations that complete the
// answer live beside the leading row and are exactly the rows diversification
// pushed below every other file. When one file clearly leads the ranked pool,
// the expendable breadth tail is re-spent on that file's next-highest-ranked
// symbols, taken from the pool as it stood before diversification.

const (
	// exploreLeadingFileDepthWindow is the ranked prefix a file must lead.
	exploreLeadingFileDepthWindow = 5
	// exploreLeadingFileDepthReserve bounds the slots depth may claim.
	exploreLeadingFileDepthReserve = 3
	// exploreLeadingFileDepthMinReserve keeps depth off pages too narrow to
	// spare a slot without collapsing breadth entirely.
	exploreLeadingFileDepthMinReserve = 2
)

func exploreLeadingFileDepthBudget(limit int) int {
	reserve := min(limit/3, exploreLeadingFileDepthReserve)
	if reserve < exploreLeadingFileDepthMinReserve {
		return 0
	}
	return reserve
}

// exploreLeadingRankedFile names the file a ranked pool has settled on, or ""
// when ranking is still scattered. The top-ranked candidate's file leads once
// the window corroborates it with a second hit; otherwise only a strict
// majority of the window counts, which at most one file can hold. A window in
// which no file repeats is decided — if at all — by the task's own path probes.
func exploreLeadingRankedFile(ranked []*rerank.Candidate) string {
	files := make([]string, 0, exploreLeadingFileDepthWindow)
	counts := make(map[string]int, exploreLeadingFileDepthWindow)
	probed := make(map[string]int, exploreLeadingFileDepthWindow)
	for _, candidate := range ranked {
		if len(files) >= exploreLeadingFileDepthWindow {
			break
		}
		if candidate == nil || candidate.Node == nil || candidate.Node.FilePath == "" {
			continue
		}
		file := candidate.Node.FilePath
		files = append(files, file)
		counts[file]++
		if score := explorePathProbeScore(candidate); score > probed[file] {
			probed[file] = score
		}
	}
	if len(files) == 0 {
		return ""
	}
	if counts[files[0]] > 1 {
		return files[0]
	}
	for _, file := range files {
		if counts[file]*2 > len(files) {
			return file
		}
	}
	return exploreProbeCorroboratedLeadingFile(files, probed)
}

// exploreProbeCorroboratedLeadingFile elects the one window file the task
// corroborates better than every other. Ranking has already declined to choose
// here, so a shared best score decides nothing and leaves the window scattered.
func exploreProbeCorroboratedLeadingFile(files []string, probed map[string]int) string {
	best, bestScore, tied := "", 0, false
	for _, file := range files {
		switch score := probed[file]; {
		case score > bestScore:
			best, bestScore, tied = file, score, false
		case score == bestScore && file != best:
			tied = true
		}
	}
	if bestScore <= 0 || tied {
		return ""
	}
	return best
}

// reserveExploreLeadingFileDepthTargets admits the leading file's highest-ranked
// symbols that are not already on the page, using free slots first and then the
// breadth tail past the direct-evidence reserve. Every evidence role that later
// stages protect stays untouched, so depth can only displace rows the page
// already treats as expendable. The pool is the ranked order as it stood before
// per-file diversification, which is where the demoted siblings still are.
//
// Allocation is confined to the window the evidence page actually serializes,
// so a wide max_symbols request keeps both its extra rows and its response
// width.
func reserveExploreLeadingFileDepthTargets(
	ranked []*rerank.Candidate,
	targets []exploreTarget,
	limit int,
	reserved map[string]struct{},
	hydrate func(*graph.Node) exploreTarget,
) []exploreTarget {
	budget := exploreLeadingFileDepthBudget(limit)
	if budget <= 0 || hydrate == nil || len(ranked) == 0 || len(targets) == 0 {
		return targets
	}
	file := exploreLeadingRankedFile(ranked)
	if file == "" {
		return targets
	}
	present := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		if target.node != nil {
			present[target.node.ID] = struct{}{}
		}
	}
	nodes := make([]*graph.Node, 0, budget)
	for _, candidate := range ranked {
		if len(nodes) >= budget {
			break
		}
		if candidate == nil || candidate.Node == nil || candidate.Node.FilePath != file {
			continue
		}
		if _, exists := present[candidate.Node.ID]; exists {
			continue
		}
		present[candidate.Node.ID] = struct{}{}
		nodes = append(nodes, candidate.Node)
	}
	if len(nodes) == 0 {
		return targets
	}

	protected := make(map[string]struct{}, len(reserved)+1)
	for id := range reserved {
		protected[id] = struct{}{}
	}
	if targets[0].node != nil {
		protected[targets[0].node.ID] = struct{}{}
	}
	pageLimit := min(limit, localizationReplayEvidenceLimit)
	pageEnd := min(len(targets), pageLimit)
	free := min(max(pageLimit-len(targets), 0), len(nodes))
	// Only the tail past the direct-evidence reserve is expendable — the same
	// rows relationship interleaving already reclaims.
	evicted := make(map[int]struct{}, len(nodes)-free)
	for index := pageEnd - 1; index >= localizationDirectEvidenceReserve && free+len(evicted) < len(nodes); index-- {
		if exploreFoldedTargetMandatory(targets[index], protected) {
			continue
		}
		evicted[index] = struct{}{}
	}
	admit := free + len(evicted)
	if admit <= 0 {
		return targets
	}
	if admit < len(nodes) {
		nodes = nodes[:admit]
	}

	page := make([]exploreTarget, 0, len(targets)+len(nodes))
	for index := 0; index < pageEnd; index++ {
		if _, drop := evicted[index]; drop {
			continue
		}
		page = append(page, targets[index])
	}
	for _, node := range nodes {
		depth := hydrate(node)
		if depth.node == nil {
			continue
		}
		depth.leadingFileDepth = true
		page = append(page, depth)
	}
	return append(page, targets[pageEnd:]...)
}

// hydrateExploreLeadingFileDepthTarget gives an admitted depth row the same
// bounded body and depth-1 neighborhood every other page row carries, at a
// fixed per-row cost bounded by exploreLeadingFileDepthReserve.
func (s *Server) hydrateExploreLeadingFileDepthTarget(
	ctx context.Context,
	eng *query.Engine,
	node *graph.Node,
	ringOpts query.QueryOptions,
) exploreTarget {
	target := exploreTarget{node: node}
	if node == nil || eng == nil {
		return target
	}
	target.source = s.manifestSymbolSource(ctx, node)
	if callers := eng.GetCallers(node.ID, ringOpts); callers != nil {
		target.callers = ringNeighbors(callers.Nodes, node.ID, exploreRingCap)
	}
	if callees := eng.GetCallChain(node.ID, ringOpts); callees != nil {
		complete := !callees.Truncated && !callees.BudgetHit && !callees.LowerBound
		var projected bool
		target.callees, projected = ringNeighborsProjection(callees.Nodes, node.ID, exploreRingCap)
		target.directCalleesComplete = complete && projected
	}
	return target
}
