package graph

// Duplicating a repository's subgraph under a new prefix.
//
// A git worktree of an already-tracked repository, checked out at the same
// commit, is the same body of code under a different prefix. Indexing and
// deriving it from scratch reproduces a subgraph the store already holds —
// measured on a five-repo Odoo workspace at 162s to index and 534s to derive,
// against 57s to copy the rows.
//
// The copy is only sound because it is ADDITIVE. Every write is an insert
// under the destination prefix that yields to an existing row; nothing is
// updated. A repository's globally-keyed nodes (`http::`, `external::`,
// `unresolved::`) keep their ids, so they collide with the rows already there
// and are skipped rather than overwritten — which is what keeps a copy from
// touching any other repository. Do not reason about this together with an
// in-place re-key, which has the opposite failure mode.

// RepoSubgraphCopyResult reports what a copy moved.
type RepoSubgraphCopyResult struct {
	Nodes    int
	Edges    int
	Sidecars int
}

// RepoSubgraphCopier duplicates one repository's nodes, edges and
// prefix-keyed sidecar rows under a new prefix.
//
// Implementations MUST rewrite both prefixed id grammars — `<prefix>/…` for
// file-derived nodes and `<prefix>::…` for the synthetic stdlib / module /
// builtin nodes — and MUST anchor on those two forms rather than on the bare
// prefix. A bare-prefix match swallows sibling checkouts: `local@wt/…` starts
// with `local`, and rewriting it merges two checkouts into one. Handling only
// the `/` grammar is the subtler failure: on one real workspace it left 15,101
// edges pointing at the SOURCE checkout's synthetic nodes, which is precisely
// the cross-checkout contamination checkout groups exist to prevent.
type RepoSubgraphCopier interface {
	CopyRepoSubgraph(srcPrefix, dstPrefix string) (RepoSubgraphCopyResult, error)
}

// CopyRepoSubgraph selects the capability when the backend has it. It reports
// false when the store cannot copy, so callers fall back to indexing rather
// than silently installing an empty repository.
func CopyRepoSubgraph(s Store, srcPrefix, dstPrefix string) (RepoSubgraphCopyResult, bool, error) {
	copier, ok := s.(RepoSubgraphCopier)
	if !ok {
		return RepoSubgraphCopyResult{}, false, nil
	}
	res, err := copier.CopyRepoSubgraph(srcPrefix, dstPrefix)
	return res, true, err
}
