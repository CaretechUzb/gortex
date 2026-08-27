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

// worktreeCopySource names a tracked repository this path may be copied from:
// a different checkout of the same repository, at the same commit.
//
// Both conditions are load-bearing. Same checkout group means the two are the
// same repository, so the destination is entitled to the source's bindings.
// Same HEAD means the same content — a worktree on another branch shares most
// files but not all, and copying it would install a graph that describes code
// that is not on disk.
func (mi *MultiIndexer) worktreeCopySource(absPath string) (string, bool) {
	if mi == nil || mi.graph == nil {
		return "", false
	}
	if !ResolveWorktree(absPath).IsWorktree {
		return "", false
	}
	group := resolvedMainRepo(absPath)
	if group == "" {
		return "", false
	}
	head := gitHeadSHA(absPath)
	if head == "" {
		return "", false
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
	for _, prefix := range prefixes {
		root := candidates[prefix]
		if resolvedMainRepo(root) != group {
			continue
		}
		if gitHeadSHA(root) != head {
			continue
		}
		return prefix, true
	}
	return "", false
}

// restatWorktreeMtimes re-reads from disk the mtimes of the files the copy
// brought over.
//
// The copied file_mtimes rows carry the SOURCE checkout's mtimes, and
// `git worktree add` writes fresh ones, so leaving them would make the next
// warm restart consider every file changed and re-index the whole repository —
// giving back exactly what the copy saved. Only paths the copy knows about are
// stat'd, so this is bounded by the repository and needs none of the indexer's
// file-discovery rules.
func restatWorktreeMtimes(root string, copied map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(copied))
	for rel := range copied {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			continue // deleted in this worktree; reconcile evicts it
		}
		out[rel] = info.ModTime().UnixNano()
	}
	return out
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
	src, ok := mi.worktreeCopySource(absPath)
	if !ok || src == prefix {
		return nil, false, nil
	}

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
	mtimes := restatWorktreeMtimes(absPath, reader.LoadFileMtimes(prefix))
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
			zap.Int("sidecar_rows", res.Sidecars),
			zap.Int("files", len(mtimes)))
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
	// it, since the copy deliberately schedules no derivation to republish it.
	mi.publishCheckoutGroups()
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
