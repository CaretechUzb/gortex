package indexer

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/graph"
)

// Installing a worktree by copying its sibling's subgraph.
//
// A git worktree of an already-tracked repository, checked out at the same
// commit, is the same body of code under a different prefix. Indexing it
// re-parses files the store has already parsed, and the post-track derivation
// then re-derives edges the store already holds — measured on a five-repo
// Odoo workspace at 162s and 534s respectively, against ~60s to copy the rows.
//
// So when the two checkouts agree on HEAD, the subgraph is duplicated instead.
// Nothing is derived afterwards: the copied edges arrive already bound, and
// the bindings a sibling checkout is ALLOWED to have are exactly the ones the
// source has — cross-repo edges into other repositories keep their targets,
// and no edge may cross between two checkouts of one repository anyway (see
// graph/checkout_groups.go), so there is nothing for a derivation to add.

// gitHeadSHA reads a checkout's HEAD. Empty when the path is not a checkout
// or git cannot answer, which disables the copy path rather than guessing.
func gitHeadSHA(root string) string {
	cmd := exec.Command("git", "-C", root, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// worktreeCopyMaxDivergence caps how far a destination checkout may sit from
// its copy source and still be installed by copy plus a targeted reconcile.
//
// There is no cliff here — the copy saves a whole-repository parse and a
// whole-repository derive, and the reconcile pays back roughly per changed
// file, so the crossover is far above any review-sized branch. The cap is
// deliberately conservative rather than fitted: copy + reconcile is measured
// only in the tens of files (a 39-path reconcile of this shape took 57.8s
// against 667s for the cold path), and a bound nobody has measured past should
// not be set where it looks precise. Above it, indexing is always correct and
// only slower.
//
// Not to be confused with the ~3,800-file ceiling where fsnotify overflows and
// the WATCHER escalates to a full-tree reconcile. That is a different mechanism
// on a different path: this reconcile is driven from an explicit file set, not
// from watch events, so it cannot overflow. The two numbers are unrelated.
//
// A var rather than a const only so a test can exercise the boundary without
// committing a thousand files. Nothing outside tests writes it.
var worktreeCopyMaxDivergence = 1000

// gitChangedPaths lists the repository-relative paths that differ between two
// commits, plus anything uncommitted in the destination's working tree.
//
// The uncommitted half matters as much as the committed one. The copy installs
// the SOURCE's graph, so any file the destination has locally modified is a
// file whose graph describes code that is not on disk — the exact hazard the
// same-HEAD gate used to exclude wholesale. Reporting them as changed routes
// them through the same reconcile as a committed difference.
//
// Reports false when git cannot answer, which declines the copy rather than
// guessing at a file set. Guessing low here is the dangerous direction: a path
// left out of this list is one the reconcile never revisits, so it would keep
// the source's nodes forever.
func gitChangedPaths(root, from, to string) ([]string, bool) {
	seen := map[string]bool{}
	collect := func(args ...string) bool {
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).Output()
		if err != nil {
			return false
		}
		for _, line := range strings.Split(string(out), "\n") {
			if rel := strings.TrimSpace(line); rel != "" {
				seen[rel] = true
			}
		}
		return true
	}
	if !collect("diff", "--name-only", from, to) {
		return nil, false
	}
	// Untracked files included: a new file in the worktree is absent from the
	// copied graph and must still reach the reconcile.
	if !collect("ls-files", "--modified", "--others", "--deleted", "--exclude-standard") {
		return nil, false
	}
	changed := make([]string, 0, len(seen))
	for rel := range seen {
		changed = append(changed, rel)
	}
	sort.Strings(changed)
	return changed, true
}

// worktreeCopySource names a tracked repository this path may be copied from —
// a different checkout of the same repository — and the paths on which the two
// disagree.
//
// Same checkout group is absolute: it means the two are the same repository, so
// the destination is entitled to the source's bindings, and nothing else here
// substitutes for it.
//
// Identical HEADs are the free case and are preferred whenever one is
// available: the checkouts describe the same code, so the copy stands alone and
// the returned change set is empty. Otherwise a sibling within
// worktreeCopyMaxDivergence still qualifies, and the caller reconciles exactly
// the disagreeing paths afterwards. That covers the case this gate used to
// refuse and which cost the most: a merge-request worktree branched a few
// commits off its base is 39 files from it, not 9,634, and re-parsing and
// re-deriving the whole repository to learn those 39 is what took 667s where
// copy plus reconcile takes about 200.
//
// A larger candidate never displaces a smaller one, and ties break on the
// sorted prefix, so the choice cannot depend on map iteration order.
func (mi *MultiIndexer) worktreeCopySource(absPath string) (string, []string, bool) {
	if mi == nil || mi.graph == nil {
		return "", nil, false
	}
	if !ResolveWorktree(absPath).IsWorktree {
		return "", nil, false
	}
	group := resolvedMainRepo(absPath)
	if group == "" {
		return "", nil, false
	}
	head := gitHeadSHA(absPath)
	if head == "" {
		return "", nil, false
	}

	mi.mu.RLock()
	candidates := make(map[string]string, len(mi.repos))
	for prefix, meta := range mi.repos {
		if meta != nil && prefix != "" && meta.RootPath != "" {
			candidates[prefix] = meta.RootPath
		}
	}
	mi.mu.RUnlock()

	// Deterministic across runs: several siblings may qualify, and the graph
	// must not depend on map iteration order.
	prefixes := make([]string, 0, len(candidates))
	for prefix := range candidates {
		prefixes = append(prefixes, prefix)
	}
	sort.Strings(prefixes)

	var (
		bestPrefix  string
		bestChanged []string
		found       bool
	)
	for _, prefix := range prefixes {
		root := candidates[prefix]
		if resolvedMainRepo(root) != group {
			continue
		}
		srcHead := gitHeadSHA(root)
		if srcHead == "" {
			continue
		}
		if srcHead == head {
			// Nothing beats an identical checkout, and taking it here keeps
			// the historical path free of any git diff at all.
			return prefix, nil, true
		}
		changed, ok := gitChangedPaths(absPath, srcHead, head)
		if !ok || len(changed) > worktreeCopyMaxDivergence {
			continue
		}
		if !found || len(changed) < len(bestChanged) {
			bestPrefix, bestChanged, found = prefix, changed, true
		}
	}
	return bestPrefix, bestChanged, found
}

// restatWorktreeMtimes re-reads from disk the mtimes of the files the copy
// brought over, and names the ones this checkout does not have.
//
// The copied file_mtimes rows carry the SOURCE checkout's mtimes, and
// `git worktree add` writes fresh ones, so leaving them would make the next
// warm restart consider every file changed and re-index the whole repository —
// giving back exactly what the copy saved. Only paths the copy knows about are
// stat'd, so this is bounded by the repository and needs none of the indexer's
// file-discovery rules.
//
// A path that does not exist KEEPS its entry and is reported as missing.
// ReconcileRepoCtx derives its deleted set by stat'ing the ledger it is handed
// (changedSinceMtimesCensus), so a path dropped here is one the reconcile never
// looks at — and the source's nodes for a file this checkout does not have
// would then stand under this prefix forever. Measured on a worktree eight
// files off its copy source: all 20 nodes of the single file that branch
// deleted survived the reconcile, which reported `deleted: 0`, against zero
// such ghosts in a cold-tracked control. The retained value is irrelevant and
// only membership matters — the census stats the path, finds nothing, and
// evicts.
//
// Any other stat error is NOT evidence of deletion. A permissions fault or a
// transient filesystem error would evict a file that exists, so those paths are
// dropped as before: that keeps the copied rows, which a later reconcile or
// watcher event corrects, and wrong-but-present beats confidently-deleted.
func restatWorktreeMtimes(root string, copied map[string]int64) (map[string]int64, map[string]bool) {
	out := make(map[string]int64, len(copied))
	missing := map[string]bool{}
	for rel, prior := range copied {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		switch {
		case err == nil:
			out[rel] = info.ModTime().UnixNano()
		case os.IsNotExist(err):
			out[rel] = prior
			missing[rel] = true
		}
	}
	return out, missing
}

// withholdReconciledPaths edits the copied mtime ledger so that every path the
// two checkouts disagree on gets reconciled — by reindexing it, or by evicting
// it — and never silently keeps the source's nodes.
//
// The two halves need opposite treatment, and the reason they are decided in
// one place is that they were once decided in two, each assuming the other
// handled deletions:
//
//   - A path that EXISTS here with different content must lose its entry. The
//     restat recorded what is on disk now, and ReconcileRepoCtx treats a path
//     whose recorded mtime matches disk as current — so an entry would announce
//     that a file the graph holds the SOURCE's nodes for is already up to date.
//     Absent, it reads as never indexed and is reindexed, which is the work the
//     copy trades a whole-repository parse for.
//
//   - A path that does NOT exist here must KEEP its entry. The reconcile's
//     deleted set is the subset of the ledger it cannot stat, so that entry is
//     the only thing telling it there is something to evict. Withholding it
//     reads as "never indexed" instead — and a file that is neither on disk nor
//     in the ledger is one nothing reindexes and nothing evicts, so the source's
//     nodes for it survive under this prefix forever.
//
// missing comes from restatWorktreeMtimes rather than a fresh stat, so the two
// cannot disagree about which paths exist.
func withholdReconciledPaths(mtimes map[string]int64, changed []string, missing map[string]bool) {
	for _, rel := range changed {
		if missing[rel] {
			continue
		}
		delete(mtimes, rel)
	}
}

// trackWorktreeByCopy installs prefix by duplicating a sibling checkout's
// subgraph. Reports false when the copy did not happen, so the caller falls
// back to indexing; it never leaves a half-installed repository behind,
// because ReconcileRepoCtx is what registers the repo and it runs last.
func (mi *MultiIndexer) trackWorktreeByCopy(
	ctx context.Context,
	entry config.RepoEntry,
	absPath, prefix string,
) (*IndexResult, bool, error) {
	src, changed, ok := mi.worktreeCopySource(absPath)
	if !ok || src == prefix {
		return nil, false, nil
	}

	// Publish the grouping before copying, not only after. The copy's inbound
	// pass asks the store which prefixes are sibling checkouts of the source,
	// and a store that has been told nothing answers "none" — which would let
	// a sibling's edges through and put two checkouts of one repository in
	// contact. At runtime the earlier tracks have already published it; inside
	// a cold batch they may not have.
	mi.publishCheckoutGroups()

	res, supported, err := graph.CopyRepoSubgraph(mi.graph, src, prefix)
	if err != nil || !supported || res.Nodes == 0 {
		// A refusal is not a failure: the destination may already hold rows,
		// or the backend may not implement the copy. Indexing is always
		// correct, only slower.
		if err != nil && mi.logger != nil {
			mi.logger.Info("worktree copy declined; indexing instead",
				zap.String("repo", prefix), zap.String("from", src), zap.Error(err))
		}
		return nil, false, nil
	}

	reader, canRead := mi.graph.(graph.FileMtimeReader)
	replacer, canWrite := mi.graph.(graph.FileMtimeReplacer)
	if !canRead || !canWrite {
		// Without the mtime sidecar the copy would install a repository the
		// next warm restart cannot recognise as indexed.
		mi.purgeCopiedPrefix(prefix)
		return nil, false, nil
	}
	mtimes, missing := restatWorktreeMtimes(absPath, reader.LoadFileMtimes(prefix))
	withholdReconciledPaths(mtimes, changed, missing)
	if len(mtimes) == 0 {
		// Nothing on disk matched the copied graph. Reconcile would fall back
		// to a full index anyway, and it would do so against a subgraph this
		// call just installed, so undo it and take the ordinary path.
		mi.purgeCopiedPrefix(prefix)
		return nil, false, nil
	}
	if err := replacer.ReplaceFileMtimes(prefix, mtimes); err != nil {
		mi.purgeCopiedPrefix(prefix)
		return nil, false, nil
	}

	// The restat above just advanced this checkout's content counter — a
	// worktree's on-disk mtimes differ from its source's even at the identical
	// commit — which strands every stage stamp the copy carried. Declare them
	// current for the destination now that its own file set is recorded;
	// without it a copied worktree reads "partial" forever, since the whole
	// point of the copy is that nothing will re-derive it.
	//
	// Only for an identical checkout. Once the two disagree that premise is
	// gone: a derivation IS owed, on the reconciled files, and it is scheduled
	// below. Declaring the carried stamps current here would report "ready" over
	// a graph whose changed files have been reindexed but never re-derived —
	// silent staleness, and the one direction a readiness stamp must never fail
	// in. Leaving them stranded is the honest state; the repo reads "partial"
	// for as long as that is true and the scheduled pass clears it.
	if restamper, ok := mi.graph.(graph.CopiedReadinessRestamper); ok && len(changed) == 0 {
		if err := restamper.RestampCopiedReadiness(prefix); err != nil && mi.logger != nil {
			mi.logger.Warn("worktree copy: could not declare carried stage stamps current",
				zap.String("repo", prefix), zap.Error(err))
		}
	}

	if mi.logger != nil {
		mi.logger.Info("worktree installed by subgraph copy",
			zap.String("repo", prefix),
			zap.String("from", src),
			zap.Int("nodes", res.Nodes),
			zap.Int("edges", res.Edges),
			zap.Int("inbound_edges", res.InboundEdges),
			zap.Int("sidecar_rows", res.Sidecars),
			zap.Int("files", len(mtimes)),
			zap.Int("reconciled_files", len(changed)),
			// Files the source holds and this checkout does not. Logged
			// separately because they are counted in `files` (they must stay in
			// the ledger for the reconcile to evict them) yet are the opposite
			// of the others: not reindexed, removed.
			zap.Int("evicted_files", len(missing)))
	}

	// ReconcileRepoCtx registers a repository whose nodes are already in the
	// graph and reconciles it against the filesystem. The mtimes handed over
	// are the real ones just read from disk, so it finds nothing stale and
	// installs the repository without re-indexing it.
	result, err := mi.ReconcileRepoCtx(ctx, entry, mtimes)
	if err != nil {
		return nil, false, err
	}
	// The grouping has to learn about the new checkout before anything reads
	// it, since an identical-checkout copy schedules no derivation to
	// republish it.
	mi.publishCheckoutGroups()

	// A diverged copy is the one case that still owes a derivation. The
	// reconcile above reindexed the changed files, which produces their
	// extraction edges and nothing else: every derived edge the rest of the
	// workspace owns into them — framework dispatch, cross-repo, implements,
	// test and capability edges — comes from the global passes. Skipping this
	// is precisely the silent under-binding that TrackRepoCtx's own
	// scheduleWorkspaceRederive exists to prevent, and the copy path returns
	// before reaching it.
	//
	// Scoped, and cheap: rederiveScope sees a frontier that is entirely a
	// sibling checkout of an already-tracked repository, so the passes run over
	// the frontier rather than the whole store. Safe to call here — the
	// ReconcileRepoCtx above has returned, so no topology mutation is open.
	if len(changed) > 0 {
		mi.scheduleWorkspaceRederive(prefix)
	}
	return result, true, nil
}

// purgeCopiedPrefix removes a subgraph this call installed, so a declined
// copy leaves the store exactly as it found it and the caller can index
// normally. Best-effort by design: the alternative to a failed purge is
// returning an error for a path that was only ever an optimisation.
func (mi *MultiIndexer) purgeCopiedPrefix(prefix string) {
	purger, ok := mi.graph.(interface{ PurgeRepo(string) error })
	if !ok {
		return
	}
	if err := purger.PurgeRepo(prefix); err != nil && mi.logger != nil {
		mi.logger.Warn("could not undo a declined worktree copy",
			zap.String("repo", prefix), zap.Error(err))
	}
}
