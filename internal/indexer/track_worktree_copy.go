package indexer

import (
	"context"
	"fmt"
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
// So when the source's INDEXED commit already describes this checkout, the
// subgraph is duplicated instead. Indexed, not HEAD — the rows being copied were
// written by the source's last index, and a source whose HEAD has moved since
// describes a tree neither checkout has. See copySourceCommit.
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

// copySourceCommit reports the commit a tracked repository's SUBGRAPH describes,
// and whether that subgraph is fit to be copied from at all.
//
// Deliberately not gitHeadSHA(root). The rows about to be duplicated were written
// by the last INDEX of that checkout, so the tree they describe is
// RepoIndexState.IndexedSHA; a checkout whose HEAD has moved since is one whose
// graph and working tree name different commits. Measured on the copy that
// prompted this: the source's HEAD sat 22h ahead of its indexed commit, so ranking
// on HEAD reported 273 divergent paths where the copied graph was 20 away, and 253
// files were reindexed for nothing.
//
// The cost is the smaller half. Ranking on HEAD is FAIL-OPEN: a file that differs
// between the source's indexed commit and its HEAD, but whose HEAD content matches
// this checkout, never appears in the diff — and restatWorktreeMtimes then records
// it as current, so the reconcile never revisits it and the source's stale nodes
// stand under this prefix forever.
//
// A DIRTY source is refused outright, and the reason is that Dirty is a bool.
// It records THAT the tree was dirty when indexed, never WHICH files, so no
// commit-to-commit diff can cover the difference — and asking git about the source
// TODAY answers about the tree today: a source that was dirty at index time and has
// since been committed or reverted reports clean while its graph still holds the
// uncommitted content. Not hypothetical — a probe found a copy's indexed
// content_hash matching its dirty source at 15,642 bytes while its own checkout
// held the committed file at 15,596. Covering this properly needs per-path dirty
// provenance the store does not persist, which is a RepoIndexState schema change.
//
// Reports false rather than falling back to HEAD. HEAD is the unsound proxy this
// function exists to remove, and declining costs only an ordinary index.
func (mi *MultiIndexer) copySourceCommit(prefix string) (string, bool) {
	if mi == nil || mi.graph == nil {
		return "", false
	}
	reader, ok := mi.graph.(graph.RepoIndexStateReader)
	if !ok {
		return "", false
	}
	st, found, err := reader.GetRepoIndexState(prefix)
	if err != nil || !found || st.IndexedSHA == "" || st.Dirty {
		return "", false
	}
	return st.IndexedSHA, true
}

// worktreeCopyMaxDivergence caps how far a destination checkout may sit from
// its copy source and still be installed by copy plus a targeted reconcile.
//
// There is no cliff here — the copy saves a whole-repository parse and a
// whole-repository derive, and the reconcile pays back roughly per changed
// file, so the crossover is far above any review-sized branch. The cap is
// deliberately conservative rather than fitted: a bound nobody has measured past
// should not be set where it looks precise. Above it, indexing is always correct
// and only slower.
//
// What "pays back per changed file" costs, measured:
//
//	changed  reconcile  of which derived passes  frontier after incoming
//	     39      57.8s   —                       —
//	    273     595.4s   467.7s                  481 files
//
// The 273-path run is the one that motivated copySourceCommit: it compared
// against the source's HEAD instead of its indexed commit, so 253 of those 273
// were reindexed for nothing — the honest number for that copy was 20. Read the
// table as the cost of a frontier, not as evidence about the cap.
//
// Note the derived-pass column dominates and is driven by the frontier AFTER
// incoming references are pulled in (155 changed files became a 481-file
// frontier), not by len(changed) directly.
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
// Every comparison is against the candidate's INDEXED commit, never its HEAD —
// see copySourceCommit for why, and for why a source dirty at index time is
// refused outright. A candidate whose indexed commit already equals this
// checkout's HEAD is the free case and is preferred whenever one is available:
// the copied rows describe exactly this code, so the copy stands alone and the
// returned change set is empty. Otherwise a sibling within
// worktreeCopyMaxDivergence still qualifies, and the caller reconciles exactly
// the disagreeing paths afterwards. That covers the case this gate used to
// refuse and which cost the most: a merge-request worktree branched a few
// commits off its base is 39 files from it, not 9,634, and re-parsing and
// re-deriving the whole repository to learn those 39 is what took 667s where
// copy plus reconcile takes about 200.
//
// A STALE candidate is not disqualified. Its graph is a self-consistent
// description of its indexed commit, so ranking on true divergence can
// legitimately prefer a stale sibling to a fresh one — and does: the copy this
// was written for chose a 22h-stale worktree 20 files away over the fresh main
// checkout 1,498 files away. Do not "fix" this by excluding stale sources.
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
		siblings    int
		declined    []string
	)
	for _, prefix := range prefixes {
		root := candidates[prefix]
		if resolvedMainRepo(root) != group {
			continue
		}
		siblings++
		srcSHA, ok := mi.copySourceCommit(prefix)
		if !ok {
			declined = append(declined, prefix+": no clean indexed commit")
			continue
		}
		if srcSHA == head {
			// Nothing beats a source whose rows already describe this commit,
			// and taking it here keeps that path free of any git diff at all.
			return prefix, nil, true
		}
		changed, ok := gitChangedPaths(absPath, srcSHA, head)
		if !ok {
			// The source's indexed commit does not resolve in this checkout —
			// rebased away, or garbage collected. There is no diff to compute
			// and no sound fallback, so decline; HEAD is exactly the proxy
			// copySourceCommit exists to remove.
			declined = append(declined, prefix+": indexed commit "+srcSHA+" does not resolve here")
			continue
		}
		if len(changed) > worktreeCopyMaxDivergence {
			declined = append(declined, fmt.Sprintf("%s: %d changed paths, over the %d cap",
				prefix, len(changed), worktreeCopyMaxDivergence))
			continue
		}
		if !found || len(changed) < len(bestChanged) {
			bestPrefix, bestChanged, found = prefix, changed, true
		}
	}
	// A worktree that had siblings and copied from none of them is about to pay
	// a full cold index. That is correct but slow, and without this line it is
	// indistinguishable from a worktree that never had a candidate at all.
	if !found && siblings > 0 && mi.logger != nil {
		mi.logger.Info("no sibling checkout qualified as a copy source; indexing instead",
			zap.String("path", absPath),
			zap.Int("siblings", siblings),
			zap.Strings("declined", declined))
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

	// AFTER the reconcile, never before. The restat above advanced this
	// checkout's content counter — a worktree's on-disk mtimes differ from its
	// source's even at the identical commit — which strands every stage stamp
	// the copy carried, so they have to be declared current or a copied worktree
	// reads "partial" forever with nothing scheduled to clear it.
	//
	// This used to run before ReconcileRepoCtx, and that ordering was the bug.
	// The reconcile EVICTS the files the source's ledger holds and this checkout
	// lacks, and the eviction bumps content_gen past the stamp already written:
	// measured at repo content_gen 3 against MIN(enrichment content_gen) 2, i.e.
	// permanently partial, recoverable only by a daemon restart. A stage stamp
	// must be immune to the output of the stage it covers, so it is written once
	// the mutation it describes has landed.
	//
	// Only for a source whose indexed commit already equalled this checkout's
	// HEAD. Once the two disagree a derivation IS owed, on the reconciled files,
	// and it is scheduled below; declaring the carried stamps current here would
	// report "ready" over a graph whose changed files were reindexed but never
	// re-derived — silent staleness, the one direction a readiness stamp must
	// never fail in. Leaving them stranded is the honest state.
	//
	// Skipped entirely when the reconcile returned an error: there is no landed
	// mutation to stamp over, and "partial" is then the truth.
	if restamper, ok := mi.graph.(graph.CopiedReadinessRestamper); ok && len(changed) == 0 {
		if err := restamper.RestampCopiedReadiness(prefix); err != nil && mi.logger != nil {
			mi.logger.Warn("worktree copy: could not declare carried stage stamps current",
				zap.String("repo", prefix), zap.Error(err))
		}
	}

	// The grouping has to learn about the new checkout before anything reads
	// it, since an identical-checkout copy schedules no derivation to
	// republish it.
	mi.publishCheckoutGroups()

	// A diverged copy still owes derivation and enrichment for the files the
	// reconcile touched — and only for those. The copy carried every derived
	// edge, inbound included, for the unchanged files, and ReconcileRepoCtx
	// runs the same synchronous resolve + incremental-derived tail an
	// ordinary file save uses, over exactly this divergence (its census
	// re-discovers the withheld paths, and the tail's frontier is seeded
	// from that census when the reindex plan comes back empty). When that
	// tail ran, the repair is complete before the reconcile returns.
	// Scheduling a repo-wide rederive on top of it — the previous behavior —
	// re-derived edges the copy already carried, at 373–1,034 s per track,
	// dominated by framework synthesizers whose cost tracks their corpus
	// rather than the frontier.
	//
	// So when the tail ran: declare the carried stamps current — sound for
	// the same reason it is sound in the identical case above, because the
	// changed files HAVE been re-derived — and arm enrichment for just the
	// reconciled files. The repo-wide rederive remains only as the fallback
	// for a reconcile whose tail was deferred to a batch transition or
	// replaced by a forced full retrack; that path still owes everything.
	if len(changed) > 0 {
		if copiedDivergenceRepaired(result) {
			if restamper, ok := mi.graph.(graph.CopiedReadinessRestamper); ok {
				if err := restamper.RestampCopiedReadiness(prefix); err != nil && mi.logger != nil {
					mi.logger.Warn("worktree copy: could not declare repaired stage stamps current",
						zap.String("repo", prefix), zap.Error(err))
				}
			}
			if mi.logger != nil {
				mi.logger.Info("worktree copy: divergence repaired by the reconcile's scoped tail; no workspace rederive owed",
					zap.String("repo", prefix),
					zap.Int("files", len(result.DerivedInvalidation.Files)))
			}
			// An empty frontier here is the proven-no-work case: nothing
			// was reindexed, so the carried enrichment rows are exact and
			// arming anything would re-enrich a bit-identical graph.
			if len(result.DerivedInvalidation.Files) > 0 {
				mi.scheduleCopiedRepoEnrich(prefix, result.DerivedInvalidation.Files)
			}
		} else if result.deferredTail != nil {
			// The tail did not run, but not because it could not: a batch was
			// suppressing it. That distinction is the whole difference between
			// a 27.8s scoped repair and a 3,255s repo-wide derivation, and it
			// was previously invisible here — copiedDivergenceRepaired reports
			// only that the tail did not run, so a copy tracked during daemon
			// warmup took the same fallback as one whose reconcile genuinely
			// re-indexed the world.
			//
			// So hold the tail for the batch transition instead of paying the
			// repo-wide pass. deferWorkspaceRederive is the fail-closed half:
			// the repo reads "owed" the whole time, and if the replay covers
			// nothing the deferred set is what schedules the fallback.
			// Enrichment is ARMED here and RUN after the replay, preserving the
			// derive-then-enrich order the fallback path has always had.
			mi.deferCopiedReconcileTail(prefix, result.deferredTail)
			mi.deferWorkspaceRederive(prefix)
			mi.armCopiedRepoEnrich(prefix, nil)
			if mi.logger != nil {
				mi.logger.Info("worktree copy: reconcile tail deferred to the batch transition; no repo-wide rederive scheduled",
					zap.String("repo", prefix),
					zap.Int("reconciled_files", len(changed)))
			}
		} else {
			mi.scheduleWorkspaceRederive(prefix)
			mi.scheduleCopiedRepoEnrich(prefix, nil)
		}
	}
	return result, true, nil
}

// copiedDivergenceRepaired reports whether a diverged copy's derivation debt
// is already discharged, so the copy path may restamp instead of scheduling
// the repo-wide rederive.
//
// Two ways to be square. Either the reconcile proved there was NO indexable
// work — a divergence made only of files outside the indexable set (.po
// translations, assets) reconciles to zero stale files and an empty plan, and
// the graph is bit-identical to the carried copy — or the reconcile's
// synchronous tail re-derived a non-empty frontier. Both halves of the second
// arm are load-bearing: a tail deferred to a batch transition has not run,
// and an empty frontier despite stale work means nothing was re-derived —
// either way restamping would bless a silently under-derived graph, so the
// caller falls back to the repo-wide pass.
func copiedDivergenceRepaired(result *IndexResult) bool {
	if result == nil || result.FullRetrack {
		return false
	}
	if result.StaleFileCount == 0 && result.DeletedFileCount == 0 &&
		result.DerivedInvalidation.Empty() {
		return true
	}
	return result.DerivedTailRan && len(result.DerivedInvalidation.Files) > 0
}

// scheduleCopiedRepoEnrich arms and runs semantic enrichment for a worktree
// installed by a DIVERGED copy.
//
// The copy carries the source's enrichment_state rows verbatim, and on a
// diverged copy those rows are not merely behind — they are wrong. They name
// the SOURCE's sha and its content_gen, describing content this checkout never
// had. Nothing downstream corrects them: the derivation scheduled above
// advances derive_state and never touches enrichment, and this is the only
// track path that installs a repository WITHOUT indexing its files, so the
// per-file arming a cold index relies on never happens either. The repository
// therefore reads "partial" — which blocks queries — with no way out.
//
// Measured before this existed, on a worktree 20 files off its copy source: a
// clean 910s derivation completed, and the repo then sat at MIN(enrichment
// content_gen) 1 against a repo content_gen of 4 for 801s of further daemon
// activity — including incremental derived passes and a janitor sweep —
// unchanged. Only a daemon restart recovered it, because SeedPendingEnrichAll,
// the sole caller of MaybeSeedPendingEnrich, runs exclusively from warmup.
//
// So this supplies the missing CALLER, not a new predicate.
// MaybeSeedPendingEnrich already classifies this shape correctly: the carried
// marker names the source's sha, the destination's HEAD differs, and a fresh
// worktree is clean, so it takes the "persisted enrichment incomplete" arm. In
// particular this must NOT widen EnrichmentOwed's "never ran" test to "behind
// the current content" — that would re-enrich every actively-edited repository
// on each daemon start, and TestAProviderMerelyBehindTheContentIsNotOwedAPass
// guards it.
//
// Sequenced after the derivation rather than run beside it. Enrichment takes
// minutes and must not block the track, but a whole-repo pass racing a scoped
// derive that is rewriting the same graph is not the watcher's bounded
// incremental case, and warmup orders seed → deferred passes for that reason.
//
// A destination tracked with UNCOMMITTED edits is covered, and covered here
// rather than by luck: gitChangedPaths reports the destination's dirty paths as
// changed, so len(changed) > 0 and this runs. It used to be a known gap only
// while the arming routed through MaybeSeedPendingEnrich, whose clean-tree
// requirement it could never satisfy; arming directly closed it.
//
// A dirty SOURCE is a different matter and is refused upstream — see
// copySourceCommit. It has to be: the source's carried enrichment rows describe
// a working tree, and no diff this function or its caller can compute says which
// files that was.
//
// files, when non-empty, is the exact graph-path frontier the reconcile
// re-indexed, and narrows the arming to those files. That is sound only
// because dirty sources are refused: every carried enrichment row outside the
// frontier describes content this checkout really has, and the caller has
// just declared those rows current via RestampCopiedReadiness. An empty
// files arms the whole repository — the fallback path, where nothing has
// vouched for the carried rows.
func (mi *MultiIndexer) scheduleCopiedRepoEnrich(prefix string, files []string) {
	idx := mi.armCopiedRepoEnrich(prefix, files)
	if idx == nil || idx.semanticMgr == nil {
		// Nothing here can run the pass. The armed gate costs nothing and a
		// later daemon start still honours it; returning also keeps this
		// reachable from a test with no semantic manager to build.
		return
	}
	go func() {
		mi.WaitWorkspaceRederive()
		mi.runDeferredEnrichPool([]*Indexer{idx})
	}()
}

// armCopiedRepoEnrich is scheduleCopiedRepoEnrich without the dispatch: it
// raises the durable gate and returns the indexer, leaving the caller to decide
// WHEN the pass runs.
//
// Split out for the deferred-tail path, which must not enrich before its
// derivation replay. Arming immediately anyway is deliberate: the gate is the
// only durable record that this repository owes a pass, and a transition that
// never comes must still leave a later daemon start able to see it.
func (mi *MultiIndexer) armCopiedRepoEnrich(prefix string, files []string) *Indexer {
	idx := mi.GetIndexer(prefix)
	if idx == nil {
		return nil
	}
	// Arm directly rather than routing through MaybeSeedPendingEnrich.
	//
	// That predicate exists to classify a repository at DAEMON START, where the
	// only evidence is what the store persisted, and it can conclude nothing
	// without a __repo__ completion marker. RecordRepoEnrichmentComplete no-ops
	// on a dirty tree, so a source checkout carrying one uncommitted file has
	// no marker to copy. Measured: copying from `local` — a single untracked
	// entry — left the destination holding the source's four provider rows and
	// no marker, so RepoEnrichmentMarkerState reported "not persisted", the
	// per-file ledger was empty (the copy indexed nothing), EnrichmentOwed was
	// false because the rows exist, and this returned false. Routing through it
	// made the fix a silent no-op for every dirty source — three of six repos
	// in the workspace it was written for.
	//
	// copySourceCommit now declines dirty sources outright, so that exact
	// measurement describes a copy this code can no longer perform. It stays
	// because the conclusion outlives it: this call site must not delegate a
	// question it already knows the answer to.
	//
	// This call site has to infer nothing. It KNOWS these rows were copied from
	// a different checkout and describe content this one never had, so the pass
	// is owed unconditionally. That is not a widening of the restart-time
	// predicate — it does not touch it, and the identical-copy branch never
	// reaches here, because RestampCopiedReadiness declares its carried stamps
	// current, which for identical content they are.
	if len(files) > 0 {
		idx.markPendingEnrichFiles(files)
	} else {
		idx.markPendingEnrichFull()
	}
	return idx
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
