package indexer

import (
	"path/filepath"
	"sort"

	"github.com/zzet/gortex/internal/graph"
)

// markPendingEnrichFull records that the next deferred semantic pass must use
// the repository entry point. Full work dominates any queued file frontier.
func (idx *Indexer) markPendingEnrichFull() {
	idx.deferredEnrichMu.Lock()
	idx.deferredEnrichGeneration++
	idx.deferredEnrichFull = true
	idx.deferredEnrichFiles = nil
	// A full arm reaches EnrichAll, which records the whole-repo completion
	// marker itself on a clean non-partial pass. The copied-marker promotion is
	// the scoped path's substitute for exactly that write, so leaving it armed
	// here would be a second writer of the same row on a path that already has
	// one.
	idx.copiedEnrichMarkerSHA = ""
	// Same reasoning for the repair flag: it selects a deadline for a SCOPED
	// dispatch and an escalation that a whole-repo pass already is. Leaving it
	// set would let the next scoped frontier — a watcher save — inherit both.
	idx.deferredEnrichRepair = false
	idx.pendingEnrich.Store(true)
	idx.deferredEnrichMu.Unlock()
}

// armCopiedEnrichRepair marks the pending scoped frontier as a worktree copy's
// repair, so its dispatch runs under the repo-scaled deadline and escalates
// itself to a whole-repo pass if it still does not complete.
//
// Only the copy path calls it, and only for a frontier that is still scoped —
// see armCopiedRepoEnrich, and the field comment on deferredEnrichRepair for
// why a repair is not just a large save.
func (idx *Indexer) armCopiedEnrichRepair() {
	idx.deferredEnrichMu.Lock()
	idx.deferredEnrichRepair = true
	idx.deferredEnrichMu.Unlock()
}

// deferredEnrichIsRepair peeks at the repair flag without consuming it. The
// dispatcher takes it through takeDeferredEnrichScope; this is for callers that
// only need to know whether one is queued.
func (idx *Indexer) deferredEnrichIsRepair() bool {
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	return idx.deferredEnrichRepair
}

// armCopiedEnrichMarkerPromotion records that a scoped repair pass over a
// worktree copy's divergence is entitled to promote the inherited whole-repo
// completion marker to sha (this checkout's HEAD) when it completes cleanly.
//
// Only the copy path calls it, and only after checking the inherited marker —
// see armCopiedRepoEnrich for the evidence, and the field comment on
// copiedEnrichMarkerSHA for why the entitlement is deliberately not durable.
func (idx *Indexer) armCopiedEnrichMarkerPromotion(sha string) {
	if sha == "" {
		return
	}
	idx.deferredEnrichMu.Lock()
	idx.copiedEnrichMarkerSHA = sha
	idx.deferredEnrichMu.Unlock()
}

// deferredEnrichIsFull reports whether the pending pass is currently scoped to
// the whole repository — either because it was armed that way, or because a
// scoped arm silently widened (see the legacy-marker branch in
// markPendingEnrichFiles). Callers that are about to arm a copied-marker
// promotion on the strength of "this frontier is scoped" must check this
// AFTER calling markPendingEnrichFiles, or a race between the widen and a
// stale `ev` from before it would re-entitle a promotion the widen just
// disqualified.
func (idx *Indexer) deferredEnrichIsFull() bool {
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	return idx.deferredEnrichFull
}

// takeCopiedEnrichMarkerPromotion reads and clears the entitlement in one step.
//
// One-shot by construction: the entitlement describes ONE armed frontier, and a
// later scoped pass over some other frontier — a watcher save, say — carries
// none of the evidence that made the promotion sound. Reading without clearing
// would let the second pass inherit the first one's proof.
func (idx *Indexer) takeCopiedEnrichMarkerPromotion() string {
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	sha := idx.copiedEnrichMarkerSHA
	idx.copiedEnrichMarkerSHA = ""
	return sha
}

// copiedMarkerPromotionAllowed is the promotion predicate, pulled out so it can
// be read (and tested) as one statement of what completes an inherited
// whole-repo assertion.
//
// want is the entitlement takeCopiedEnrichMarkerPromotion returned; sha and
// dirty are the repo's git state as the pass observed it BEFORE dispatching;
// withheld is the set of providers whose language was in the frontier and which
// produced nothing. Every clause is a way the two proofs stop composing: no
// entitlement, HEAD moved out from under the pass, the tree is no longer the
// committed state sha names, or some provider's edges are gone and nothing put
// them back.
func copiedMarkerPromotionAllowed(want, sha string, dirty bool, withheld []string) bool {
	return want != "" && want == sha && !dirty && len(withheld) == 0
}

// markPendingEnrichFiles merges a known repo-scoped frontier. The complete set
// is dispatched to a batch-capable provider in one call; it is never expanded
// into an N+1 loop of per-file provider calls.
func (idx *Indexer) markPendingEnrichFiles(filePaths []string) {
	if len(filePaths) == 0 {
		return
	}

	idx.deferredEnrichMu.Lock()
	idx.deferredEnrichGeneration++
	if idx.pendingEnrich.Load() && !idx.deferredEnrichFull && len(idx.deferredEnrichFiles) == 0 {
		// A caller using the legacy atomic-only marker queued work whose scope is
		// unknown. Preserve it as a full pass instead of silently narrowing it.
		//
		// Clear the copied-marker entitlement for the same reason
		// markPendingEnrichFull does: what runs after this is a whole-repo pass,
		// which writes the marker itself. This arm is unreachable from the copy
		// path today — that path installs a repository WITHOUT indexing its
		// files, so nothing has raised pendingEnrich when it arms — but if it
		// ever starts indexing first, the widening below would silently turn the
		// promotion flag into dead state that no scoped pass ever consumes.
		//
		// The repair flag goes for the same reason: it selects a deadline for a
		// scoped dispatch and an escalation to the very pass this widen just
		// scheduled.
		idx.copiedEnrichMarkerSHA = ""
		idx.deferredEnrichRepair = false
		idx.deferredEnrichFull = true
	}
	if !idx.deferredEnrichFull {
		if idx.deferredEnrichFiles == nil {
			idx.deferredEnrichFiles = make(map[string]struct{}, len(filePaths))
		}
		for _, filePath := range filePaths {
			if filePath != "" {
				idx.deferredEnrichFiles[filePath] = struct{}{}
			}
		}
	}
	idx.pendingEnrich.Store(true)
	idx.deferredEnrichMu.Unlock()
}

// deferredEnrichScope snapshots pending work. An empty frontier is always
// treated as full for compatibility with older/direct pendingEnrich writers.
//
// A pure peek: it consumes nothing, so a reader that only wants to see the
// queued frontier (the watcher's batch coalescer) cannot disarm the repair
// disposition out from under the pass that is going to run it.
func (idx *Indexer) deferredEnrichScope() (filePaths []string, full bool, generation uint64) {
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	return idx.deferredEnrichScopeLocked()
}

// takeDeferredEnrichScope is deferredEnrichScope for the dispatcher: it also
// reads and clears the copy-repair disposition, in the SAME critical section as
// the snapshot. One lock matters — a dispatch that read the frontier and the
// flag separately could run a save's frontier under a repair's deadline, or a
// repair's frontier under a save's, depending on which arm landed between the
// two reads.
func (idx *Indexer) takeDeferredEnrichScope() (filePaths []string, full bool, generation uint64, repair bool) {
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	filePaths, full, generation = idx.deferredEnrichScopeLocked()
	repair = idx.deferredEnrichRepair
	idx.deferredEnrichRepair = false
	return filePaths, full, generation, repair
}

// deferredEnrichScopeLocked is the snapshot both entry points share.
// deferredEnrichMu must be held.
func (idx *Indexer) deferredEnrichScopeLocked() (filePaths []string, full bool, generation uint64) {
	filePaths = make([]string, 0, len(idx.deferredEnrichFiles))
	for filePath := range idx.deferredEnrichFiles {
		filePaths = append(filePaths, filePath)
	}
	sort.Strings(filePaths)
	return filePaths,
		idx.deferredEnrichFull || len(filePaths) == 0,
		idx.deferredEnrichGeneration
}

// deferredEnrichFrontiers partitions one exact graph-file frontier by language
// with a single batched graph read. Deleted paths are absent and therefore need
// no provider work: their semantic nodes and edges were already evicted. The
// caller invokes each language provider once for the complete file batch.
func (idx *Indexer) deferredEnrichFrontiers(graphPaths []string) map[string][]string {
	nodesByFile := idx.graph.GetFileNodesByPaths(graphPaths)
	byLanguage := make(map[string][]string)
	for _, graphPath := range graphPaths {
		base := filepath.Base(graphPath)
		if base == "go.mod" || base == "go.work" {
			continue
		}
		for _, node := range nodesByFile[graphPath] {
			if node == nil || node.Kind != graph.KindFile || node.Language == "" {
				continue
			}
			byLanguage[node.Language] = append(byLanguage[node.Language], graphPath)
			break
		}
	}
	for language, paths := range byLanguage {
		byLanguage[language] = appendUniqueSorted(nil, paths...)
	}
	return byLanguage
}

// semanticDependencyFrontierForDeletedFiles captures the surviving source files
// whose resolvable references point at symbols about to be deleted. Every graph
// access is batched over the deletion set; the result is repo-local because a
// provider rooted at this indexer's checkout cannot safely enrich another repo.
func (idx *Indexer) semanticDependencyFrontierForDeletedFiles(relPaths []string) []string {
	if len(relPaths) == 0 {
		return nil
	}
	graphPaths := make([]string, 0, len(relPaths))
	for _, relPath := range relPaths {
		if relPath != "" {
			graphPaths = append(graphPaths, idx.prefixPath(relPath))
		}
	}
	graphPaths = appendUniqueSorted(nil, graphPaths...)
	nodesByFile := idx.graph.GetFileNodesByPaths(graphPaths)
	evictedIDs := make(map[string]struct{})
	var nodeIDs []string
	for _, graphPath := range graphPaths {
		for _, node := range nodesByFile[graphPath] {
			if node == nil || node.ID == "" {
				continue
			}
			if _, duplicate := evictedIDs[node.ID]; duplicate {
				continue
			}
			evictedIDs[node.ID] = struct{}{}
			nodeIDs = append(nodeIDs, node.ID)
		}
	}
	if len(nodeIDs) == 0 {
		return nil
	}

	incoming := idx.graph.GetInEdgesByNodeIDs(nodeIDs)
	sourceSet := make(map[string]struct{})
	for _, nodeID := range nodeIDs {
		for _, edge := range incoming[nodeID] {
			if edge == nil || !graph.IsResolvableRefEdge(edge.Kind) || graph.IsUnresolvedTarget(edge.To) {
				continue
			}
			if _, deleted := evictedIDs[edge.From]; !deleted {
				sourceSet[edge.From] = struct{}{}
			}
		}
	}
	if len(sourceSet) == 0 {
		return nil
	}
	sourceIDs := make([]string, 0, len(sourceSet))
	for id := range sourceSet {
		sourceIDs = append(sourceIDs, id)
	}
	sources := idx.graph.GetNodesByIDs(sourceIDs)
	var frontier []string
	for _, id := range sourceIDs {
		node := sources[id]
		if node == nil || node.FilePath == "" || node.RepoPrefix != idx.repoPrefix {
			continue
		}
		frontier = append(frontier, node.FilePath)
	}
	return appendUniqueSorted(nil, frontier...)
}

// pendingEnrichFrontier reads the DURABLE per-file deferral ledger: the graph
// paths whose KindFile node still carries graph.MetaReparsePendingEnrichment,
// which the incremental / watch paths stamp on a file they re-parsed while
// semantic enrichment was deferred (semantic.enrich_on_watch=false, or a
// deferred global-pass window). Unlike the in-memory deferredEnrichFiles set it
// lives on the node itself, so it survives a daemon restart — and unlike the
// whole-repo completion marker it needs neither a git sha nor a clean tree, so
// it is the ONLY evidence of outstanding enrichment for a repo that is not a
// git checkout or whose watch-indexed edits are still uncommitted.
//
// One repo-scoped, kind-filtered projection per call; the marker is a binary
// meta blob, so the key test is decoded backend-side rather than pushed into
// the query. Bounded by the repo's file count, not its symbol count.
func (idx *Indexer) pendingEnrichFrontier() []string {
	nodes := graph.ReadRepoNodesByKindsWithMetaKey(
		idx.graph, idx.repoPrefix, idx.workspaceID,
		[]graph.NodeKind{graph.KindFile}, graph.MetaReparsePendingEnrichment,
	)
	var frontier []string
	for _, node := range nodes {
		if node == nil || node.FilePath == "" {
			continue
		}
		// Mirror suppressionMayBeStale: only a truthy marker is pending. The
		// writer deletes the key when it clears, so a false value is unreachable
		// today — reading it defensively keeps the two consumers in agreement.
		if pending, _ := node.Meta[graph.MetaReparsePendingEnrichment].(bool); pending {
			frontier = append(frontier, node.FilePath)
		}
	}
	return appendUniqueSorted(nil, frontier...)
}

// dischargePendingEnrichFrontier clears the durable per-file deferral for the
// graph paths a deferred pass just covered. Without it the ledger only ever
// grows: nothing but a watch-time enrichment (which is off by construction on
// the deferral path) cleared the marker, so a resumed pass would re-arm the
// same files on every restart, and find_usages would keep riding every one of
// those files' suppressions with a re-verification-pending flag that no
// completed enrichment could ever retire.
func (idx *Indexer) dischargePendingEnrichFrontier(graphPaths []string) {
	if len(graphPaths) == 0 {
		return
	}
	discharged := make(map[string]bool, len(graphPaths))
	for _, graphPath := range graphPaths {
		if graphPath != "" {
			discharged[graphPath] = false
		}
	}
	idx.setReparsePendingEnrichments(discharged)
}

// clearPendingEnrich clears only the work represented by generation. If a
// watcher queued another change during enrichment, that newer work remains.
func (idx *Indexer) clearPendingEnrich(generation uint64) bool {
	idx.deferredEnrichMu.Lock()
	defer idx.deferredEnrichMu.Unlock()
	if idx.deferredEnrichGeneration != generation {
		return false
	}
	idx.deferredEnrichFiles = nil
	idx.deferredEnrichFull = false
	idx.pendingEnrich.Store(false)
	return true
}
