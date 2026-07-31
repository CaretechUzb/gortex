package resolver

import "github.com/zzet/gortex/internal/graph"

// WorkspaceMembership maps a graph file path to the package-manager
// workspace that owns it. A monorepo is often a package-manager
// workspace — a root manifest (npm/yarn package.json, pnpm-workspace.yaml,
// Cargo.toml) that declares a set of member packages. Two member
// packages can legitimately define a symbol or package of the same
// name; when import resolution finds several same-named candidates the
// resolver prefers the one in the importing file's own workspace.
//
// Given a repo-prefixed graph file path the implementation returns an
// opaque workspace identifier — any stable string that is equal for two
// files in the same workspace and unequal across workspaces. It returns
// "" when the file belongs to no detected workspace (a plain repo, or a
// file outside every member directory); callers treat "" as "no
// workspace signal" and fall back to their pre-existing tie-break.
//
// The type lives in the resolver package so the resolver has no
// compile-time dependency on the indexer or the filesystem — the
// indexer builds a concrete implementation (which reads the workspace
// manifests from disk) and injects it via SetWorkspaceMembership.
type WorkspaceMembership func(filePath string) string

// SetWorkspaceMembership installs the package-manager workspace lookup
// used to break same-named import collisions. Pass nil to detach. Must
// be called before ResolveAll / ResolveFile — the resolver caches no
// workspace state across passes, so a mid-pass swap would race the
// parallel resolveEdge workers.
func (r *Resolver) SetWorkspaceMembership(fn WorkspaceMembership) {
	r.workspaceMembers = fn
}

// SetWorkspaceMembership installs the package-manager workspace lookup
// on the cross-repo resolver. Same contract as the Resolver method.
func (cr *CrossRepoResolver) SetWorkspaceMembership(fn WorkspaceMembership) {
	cr.workspaceMembers = fn
}

// preferSameWorkspaceFile picks, from a set of equally-ranked file
// candidates, the one that shares the importing file's package-manager
// workspace. It returns that candidate, or nil when no workspace lookup
// is installed, the importer belongs to no workspace, or zero / more
// than one candidate matches the importer's workspace (an ambiguous or
// empty result is left to the caller's existing tie-break).
//
// This is the same-workspace-preference rule for a name collision:
// when an import of a bare name (`logger`) matches a directory in two
// different workspace members, the member that belongs to the
// importer's own workspace wins.
func (r *Resolver) preferSameWorkspaceFile(callerFile string, candidates []graph.FileNodeIdentity) (graph.FileNodeIdentity, bool) {
	return pickSameWorkspaceFile(r.workspaceMembers, callerFile, candidates)
}

// preferSameWorkspaceFile is the CrossRepoResolver counterpart — same
// rule, same contract.
func (cr *CrossRepoResolver) preferSameWorkspaceFile(callerFile string, candidates []graph.FileNodeIdentity) (graph.FileNodeIdentity, bool) {
	return pickSameWorkspaceFile(cr.workspaceMembers, callerFile, candidates)
}

// pickSameWorkspaceFile is the shared body of the two
// preferSameWorkspaceFile methods.
func pickSameWorkspaceFile(fn WorkspaceMembership, callerFile string, candidates []graph.FileNodeIdentity) (graph.FileNodeIdentity, bool) {
	if fn == nil || callerFile == "" || len(candidates) < 2 {
		return graph.FileNodeIdentity{}, false
	}
	callerWS := fn(callerFile)
	if callerWS == "" {
		return graph.FileNodeIdentity{}, false
	}
	var match graph.FileNodeIdentity
	found := false
	for _, file := range candidates {
		if fn(file.FilePath) != callerWS {
			continue
		}
		if found {
			// More than one candidate sits in the importer's
			// workspace — the workspace does not disambiguate, so
			// defer to the caller's existing tie-break.
			return graph.FileNodeIdentity{}, false
		}
		match, found = file, true
	}
	return match, found
}
