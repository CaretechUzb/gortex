package indexer

import (
	"context"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/config"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/pathkey"
	"github.com/zzet/gortex/internal/resolver"
)

// resolveWorkspaceID returns the workspace slug for an indexer
// emitting nodes for the repo with the given fallback prefix.
//
// Resolution order (highest priority first):
//
//  1. RepoEntry.Workspace — user-level override in
//     `~/.gortex/config.yaml`. Lets users pin OSS / read-only
//     repos to a workspace without leaving a `.gortex.yaml` artifact
//     in the repo itself, and lets users override a workspace the
//     repo author chose (the OSS author's slug shouldn't pollute the
//     user's local graph identity).
//  2. `.gortex.yaml::workspace:` — source of truth when the user is
//     the same person who owns the repo (typical for first-party /
//     monorepo setups).
//  3. Falls back to `fallbackPrefix` (the repo prefix). This is the
//     "files without a workspace declaration get workspace =
//     repo-name" default.
//
// Empty `fallbackPrefix` returns "" — single-repo callers that don't
// pass a prefix accept that the workspace slug stays empty, in which
// case the boundary checks treat the node as "no workspace declared"
// and degrade to the legacy RepoPrefix-keyed comparison.
func resolveWorkspaceID(entry *config.RepoEntry, cfg *config.Config, fallbackPrefix string) string {
	if entry != nil && entry.Workspace != "" {
		return entry.Workspace
	}
	if cfg != nil && cfg.Workspace != "" {
		return cfg.Workspace
	}
	return fallbackPrefix
}

// resolveProjectID returns the project slug for an indexer emitting
// nodes for the repo with the given fallback prefix.
//
// Resolution order mirrors resolveWorkspaceID:
//
//  1. RepoEntry.Project — global config override.
//  2. `.gortex.yaml::project:`.
//  3. Per-file `projects[]` paths-glob lookup is the monorepo case.
//     The current implementation stamps a single project slug per
//     repo; the per-file refinement is a follow-up.
//  4. Falls back to `fallbackPrefix` (the repo prefix). Same
//     "missing → repo-name" rule as workspace.
func resolveProjectID(entry *config.RepoEntry, cfg *config.Config, fallbackPrefix string) string {
	if entry != nil && entry.Project != "" {
		return entry.Project
	}
	if cfg != nil && cfg.Project != "" {
		return cfg.Project
	}
	return fallbackPrefix
}

// ScopeForCWD resolves a working directory to the (workspace, project)
// scope of the tracked repo that physically contains it. The longest
// matching repo root wins so a repo nested inside another resolves to
// the inner one.
//
// ok is false when cwd lies outside every tracked repo — callers MUST
// fail closed (return no cross-workspace data) rather than widening to
// the global graph.
//
// A repo whose effective WorkspaceID is empty (no `.gortex.yaml::
// workspace:` and no global-config override) is treated as its own
// singleton workspace keyed on the repo prefix — the same fallback
// rule resolveWorkspaceID and QueryOptions.scopeAllows use, so the
// session boundary stays consistent with the node stamps.
func (mi *MultiIndexer) ScopeForCWD(cwd string) (workspaceID, projectID, repoPrefix string, ok bool) {
	if mi == nil || cwd == "" {
		return "", "", "", false
	}
	cwd = filepath.Clean(cwd)

	mi.mu.RLock()
	defer mi.mu.RUnlock()

	var bestRoot, bestPrefix string
	for prefix, meta := range mi.repos {
		if meta == nil {
			continue
		}
		root := filepath.Clean(meta.RootPath)
		if root == "" || root == "." {
			continue
		}
		if pathkey.HasPathPrefix(cwd, root) {
			if len(root) > len(bestRoot) {
				bestRoot, bestPrefix = root, prefix
			}
		}
	}
	if bestPrefix == "" {
		return "", "", "", false
	}

	ws, proj := bestPrefix, bestPrefix
	if idx := mi.indexers[bestPrefix]; idx != nil {
		if v := idx.WorkspaceID(); v != "" {
			ws = v
		}
		if v := idx.ProjectID(); v != "" {
			proj = v
		}
	}
	return ws, proj, bestPrefix, true
}

// ReposInWorkspace returns the set of repo prefixes whose effective
// workspace slug equals workspaceID. It is the complete list of repos
// a workspace-scoped session is permitted to see — used to bound the
// query surface for handlers that filter by repo prefix rather than
// routing through the engine's WorkspaceID-aware traversal.
//
// The effective slug follows the same singleton fallback as
// ScopeForCWD: a repo with no declared workspace is its own workspace
// keyed on the repo prefix.
func (mi *MultiIndexer) ReposInWorkspace(workspaceID string) map[string]bool {
	out := make(map[string]bool)
	if mi == nil || workspaceID == "" {
		return out
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	for prefix := range mi.repos {
		ws := prefix
		if idx := mi.indexers[prefix]; idx != nil {
			if v := idx.WorkspaceID(); v != "" {
				ws = v
			}
		}
		if ws == workspaceID {
			out[prefix] = true
		}
	}
	return out
}

// BackfillWorkspaceSlugs walks every node and contract attached to
// the MultiIndexer's tracked repos and stamps WorkspaceID / ProjectID
// from the per-repo `.gortex.yaml` whenever those fields are empty.
//
// This closes the upgrade gap: a snapshot written by a daemon
// before WorkspaceID existed has WorkspaceID="" everywhere; gob
// decodes additive fields as zero. Without backfill, EffectiveWorkspace
// falls back to RepoPrefix and explicit shared-workspace setups
// (multiple repos declaring `workspace: shared`) silently lose
// identity until every file is touched. This pass re-stamps them
// once at warmup.
//
// Returns the count of nodes and contracts updated for telemetry.
// Idempotent: re-running on an already-stamped graph is a no-op.
func (mi *MultiIndexer) BackfillWorkspaceSlugs() (nodesStamped, contractsStamped int) {
	nodesStamped, contractsStamped, _ = mi.backfillWorkspaceSlugsWithImpact()
	return nodesStamped, contractsStamped
}

// backfillWorkspaceSlugsWithImpact retains public changed-row telemetry and
// separately reports stamps that can alter cross-workspace eligibility.
func (mi *MultiIndexer) backfillWorkspaceSlugsWithImpact() (nodesStamped, contractsStamped, resolutionAffected int) {
	if mi == nil || mi.graph == nil || mi.configMgr == nil {
		return 0, 0, 0
	}
	mi.mu.RLock()
	repoMeta := make(map[string]string, len(mi.repos))
	for prefix, meta := range mi.repos {
		if meta != nil {
			repoMeta[prefix] = meta.RootPath
		}
	}
	mi.mu.RUnlock()

	type slugs struct{ ws, proj string }
	bySlug := make(map[string]slugs, len(repoMeta))
	// Map each repo prefix to its global-config RepoEntry so we honour
	// the user-level workspace/project override even on backfill.
	entryByPrefix := make(map[string]config.RepoEntry, len(repoMeta))
	if mi.configMgr.Global() != nil {
		for _, e := range mi.configMgr.Global().Repos {
			p := config.ResolvePrefix(e)
			if p == "" || p == "." {
				continue
			}
			entryByPrefix[p] = e
		}
	}
	for prefix, root := range repoMeta {
		// Make sure the per-repo `.gortex.yaml` is loaded — at warmup
		// time TrackRepoCtx/ReconcileRepoCtx already calls this, but
		// run defensively in case BackfillWorkspaceSlugs is invoked
		// on a path that didn't.
		mi.configMgr.LoadWorkspaceConfig(prefix, root)
		cfg := mi.configMgr.GetRepoConfig(prefix)
		var entryPtr *config.RepoEntry
		if e, ok := entryByPrefix[prefix]; ok {
			entryCopy := e
			entryPtr = &entryCopy
		}
		bySlug[prefix] = slugs{
			ws:   resolveWorkspaceID(entryPtr, cfg, prefix),
			proj: resolveProjectID(entryPtr, cfg, prefix),
		}
	}

	slugRows := make([]graph.WorkspaceSlug, 0, len(bySlug))
	for prefix, s := range bySlug {
		slugRows = append(slugRows, graph.WorkspaceSlug{
			RepoPrefix: prefix,
			Workspace:  s.ws,
			Project:    s.proj,
		})
	}
	sort.Slice(slugRows, func(i, j int) bool { return slugRows[i].RepoPrefix < slugRows[j].RepoPrefix })
	if backfiller, ok := mi.graph.(graph.WorkspaceSlugImpactBackfiller); ok {
		result := backfiller.BackfillWorkspaceSlugsWithImpact(slugRows)
		nodesStamped = result.Changed
		resolutionAffected = result.ResolutionAffected
	} else if backfiller, ok := mi.graph.(graph.WorkspaceSlugBackfiller); ok {
		nodesStamped = backfiller.BackfillWorkspaceSlugs(slugRows)
		// Unknown implementations do not expose the effective-boundary delta;
		// fail closed and preserve the former full-resolve behavior.
		resolutionAffected = nodesStamped
	}

	// Per-repo contract registries: rehydrated from snapshot they
	// carry RepoPrefix but no Workspace/Project slugs.
	mi.mu.RLock()
	indexers := make(map[string]*Indexer, len(mi.indexers))
	for k, v := range mi.indexers {
		indexers[k] = v
	}
	mi.mu.RUnlock()
	for prefix, idx := range indexers {
		s, ok := bySlug[prefix]
		if !ok {
			continue
		}
		reg := idx.ContractRegistry()
		if reg == nil {
			continue
		}
		all := reg.All()
		fresh := make([]contracts.Contract, 0, len(all))
		dirty := false
		for _, c := range all {
			if (c.WorkspaceID == "" && s.ws != "") || (c.ProjectID == "" && s.proj != "") {
				if c.WorkspaceID == "" {
					c.WorkspaceID = s.ws
				}
				if c.ProjectID == "" {
					c.ProjectID = s.proj
				}
				dirty = true
				contractsStamped++
			}
			fresh = append(fresh, c)
		}
		if dirty {
			reg.Clear()
			for _, c := range fresh {
				reg.Add(c)
			}
		}
	}
	return nodesStamped, contractsStamped, resolutionAffected
}

// RunGlobalResolve runs a cross-repo + cross-workspace resolution
// pass over the merged graph, then reconciles contract bridge edges.
// Used post-warmup (after BackfillWorkspaceSlugs) and any other time
// the daemon needs to refresh cross-repo edges without re-indexing
// every file. Idempotent — safe to call repeatedly.
func (mi *MultiIndexer) RunGlobalResolve() {
	mi.runCrossRepoResolve(true)
}

// runCrossRepoResolve runs the cross-repo resolver over the shared graph,
// lifting references that span repo boundaries. When reconcileContracts is
// true it also reconciles contract-bridge edges; the pre-enrichment resolve
// stage (RunPreEnrichResolve) passes false because the contract pass has not
// committed its nodes yet.
//
// The pass is deliberately whole-graph. Unlike the same-repo master resolve
// (which the warm restart scopes to the changed repos), the cross-repo pass
// cannot be scoped to the changed repos' own out-edges: an INBOUND reference
// from an unchanged repo into a symbol a changed repo just added is owned by
// the unchanged repo, so a changed-repo-scoped edge set never contains it, and
// a bare unresolved name gives no hint which repo it will bind into. Resolving
// only the changed repos' out-edges here left those inbound edges unbound until
// the post-enrichment global resolve, so cross-repo queries returned incomplete
// results for the whole enrichment window even though the daemon reported ready.
func (mi *MultiIndexer) newCrossRepoResolver() *resolver.CrossRepoResolver {
	if mi == nil || mi.graph == nil {
		return nil
	}
	cr := resolver.NewCrossRepo(mi.graph)
	cr.SetLogger(mi.logger)
	cr.SetCrossWorkspaceDepLookup(mi.crossWorkspacePassLookup())
	cr.SetNpmAliasResolver(mi.npmAliasResolver())
	cr.SetPathAliasResolver(mi.pathAliasResolver())
	cr.SetWorkspaceMembership(mi.workspaceMembershipResolver())
	mi.applyRemoteStitch(cr)
	return cr
}

func (mi *MultiIndexer) runCrossRepoResolve(reconcileContracts bool) {
	if err := mi.runCrossRepoResolveContext(context.Background(), reconcileContracts); err != nil {
		mi.logger.Error("cross-repo resolve", zap.Error(err))
	}
}

func (mi *MultiIndexer) runCrossRepoResolveContext(ctx context.Context, reconcileContracts bool) error {
	cr := mi.newCrossRepoResolver()
	if cr == nil {
		return nil
	}
	if _, err := cr.ResolveAllContext(ctx); err != nil {
		return err
	}
	if reconcileContracts {
		mi.ReconcileContractEdges()
	}
	return nil
}

// runCrossRepoResolveFiles binds both outgoing and incoming unresolved edges
// for one coordinated mutation frontier. Its caller owns the repository lane
// and global topology-writer gate, so the graph cannot expose a half-resolved
// batch between same-repo, semantic, and cross-repository phases.
func (mi *MultiIndexer) runCrossRepoResolveFiles(filePaths []string) {
	if len(filePaths) == 0 {
		return
	}
	cr := mi.newCrossRepoResolver()
	if cr == nil {
		return
	}
	cr.ResolveFilesAndIncoming(filePaths)
}

// crossWorkspaceLookup builds a resolver.CrossWorkspaceDepLookup from
// the MultiIndexer's per-repo configs. The closure captures `mi` so a
// post-construction config reload (`Reload` on the ConfigManager) is
// picked up automatically — each call walks the current per-repo
// configs and aggregates declarations whose source workspace matches.
//
// Why iterate per-repo: the schema places `cross_workspace_deps`
// inside a repo's `.gortex.yaml`, keyed implicitly to that repo's
// `workspace`. Two repos can both declare workspace = "tuck" with
// overlapping but possibly extended dep lists; the union forms the
// effective rule set for source workspace "tuck".
type crossWorkspaceRuleSnapshot struct {
	revision         uint64
	repoCount        int
	rulesByWorkspace map[string][]resolver.CrossWorkspaceDepRule
}

func (mi *MultiIndexer) buildCrossWorkspaceRuleSnapshot() *crossWorkspaceRuleSnapshot {
	if mi == nil || mi.configMgr == nil {
		return &crossWorkspaceRuleSnapshot{rulesByWorkspace: make(map[string][]resolver.CrossWorkspaceDepRule)}
	}

	for {
		revision := mi.configMgr.Revision()
		rulesByWorkspace, repoCount := mi.collectCrossWorkspaceRules()
		if revision == mi.configMgr.Revision() {
			return &crossWorkspaceRuleSnapshot{
				revision:         revision,
				repoCount:        repoCount,
				rulesByWorkspace: rulesByWorkspace,
			}
		}
	}
}

func (mi *MultiIndexer) crossWorkspaceRepoCount() int {
	if mi == nil {
		return 0
	}
	mi.mu.RLock()
	count := len(mi.repos)
	mi.mu.RUnlock()
	return count
}

func (mi *MultiIndexer) collectCrossWorkspaceRules() (map[string][]resolver.CrossWorkspaceDepRule, int) {
	rulesByWorkspace := make(map[string][]resolver.CrossWorkspaceDepRule)
	mi.mu.RLock()
	repoPrefixes := make([]string, 0, len(mi.repos))
	for prefix := range mi.repos {
		repoPrefixes = append(repoPrefixes, prefix)
	}
	repoCount := len(mi.repos)
	mi.mu.RUnlock()
	sort.Strings(repoPrefixes)
	// Per-repo crossWorkspaceLookup needs the same precedence as the
	// stamp path: a global-config Workspace override changes which
	// source-workspace bucket receives the repo's dependency rules.
	entryByPrefix := make(map[string]config.RepoEntry)
	if global := mi.configMgr.Global(); global != nil {
		for _, e := range global.Repos {
			p := config.ResolvePrefix(e)
			if p == "" || p == "." {
				continue
			}
			entryByPrefix[p] = e
		}
	}
	for _, prefix := range repoPrefixes {
		cfg := mi.configMgr.GetRepoConfig(prefix)
		if cfg == nil {
			continue
		}
		var entryPtr *config.RepoEntry
		if e, ok := entryByPrefix[prefix]; ok {
			entryCopy := e
			entryPtr = &entryCopy
		}
		repoWS := resolveWorkspaceID(entryPtr, cfg, prefix)
		for _, dep := range cfg.CrossWorkspaceDeps {
			if dep.Workspace == "" {
				continue
			}
			rulesByWorkspace[repoWS] = append(rulesByWorkspace[repoWS], resolver.CrossWorkspaceDepRule{
				Workspace: dep.Workspace,
				Modules:   append([]string(nil), dep.Modules...),
			})
		}
	}
	return rulesByWorkspace, repoCount
}

// crossWorkspacePassLookup freezes one immutable config snapshot for an
// individual cross-repository pass, so a concurrent reload cannot change
// eligibility rules halfway through that pass.
func (mi *MultiIndexer) crossWorkspacePassLookup() resolver.CrossWorkspaceDepLookup {
	snapshot := mi.buildCrossWorkspaceRuleSnapshot()
	return func(sourceWS string) []resolver.CrossWorkspaceDepRule {
		return snapshot.rulesByWorkspace[sourceWS]
	}
}

// crossWorkspaceLookup is the long-lived watcher lookup. It keeps its hot path
// allocation-free while rebuilding the immutable snapshot once per config
// revision so ConfigManager.Reload remains visible without watcher restart.
func (mi *MultiIndexer) crossWorkspaceLookup() resolver.CrossWorkspaceDepLookup {
	if mi == nil || mi.configMgr == nil {
		return func(string) []resolver.CrossWorkspaceDepRule { return nil }
	}

	var current atomic.Pointer[crossWorkspaceRuleSnapshot]
	var refreshMu sync.Mutex
	current.Store(mi.buildCrossWorkspaceRuleSnapshot())

	return func(sourceWS string) []resolver.CrossWorkspaceDepRule {
		snapshot := current.Load()
		configRevision := mi.configMgr.Revision()
		repoCount := mi.crossWorkspaceRepoCount()
		if snapshot.revision != configRevision || snapshot.repoCount != repoCount {
			refreshMu.Lock()
			snapshot = current.Load()
			configRevision = mi.configMgr.Revision()
			repoCount = mi.crossWorkspaceRepoCount()
			if snapshot.revision != configRevision || snapshot.repoCount != repoCount {
				snapshot = mi.buildCrossWorkspaceRuleSnapshot()
				current.Store(snapshot)
			}
			refreshMu.Unlock()
		}
		// The snapshot is immutable after construction. Resolver consumers
		// must treat the returned rules and their Modules slices as read-only;
		// returning them directly keeps this hot lookup allocation-free.
		return snapshot.rulesByWorkspace[sourceWS]
	}
}
