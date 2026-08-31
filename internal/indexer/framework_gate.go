package indexer

import (
	"sort"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/frameworkgate"
	"github.com/zzet/gortex/internal/resolver"
)

// allowedFrameworks resolves this repository's index.frameworks.allow
// list. Single-repository paths (the full index settle point and
// ResolveAll) are exact: one config, one graph, no ambiguity.
func (idx *Indexer) allowedFrameworks() frameworkgate.Set {
	return idx.config.AllowedFrameworks()
}

// allowedFrameworks resolves which passes the workspace-wide synthesis
// run EXECUTES, by UNIONING every tracked repository's list — a pass runs
// when any repository allows it.
//
// The union governs execution only, never where the edges land. The pass
// writes into one shared graph, so narrowing execution on one
// repository's say-so would also strip a sibling repository's edges, and
// that sibling never opted out. Which repositories a running pass may
// write into is answered separately, per edge, by allowedFrameworksByRepo
// below — so a repository that excluded a framework is protected even
// though a sibling kept the pass alive.
//
// Scoping the fold to the *changed* prefixes instead was rejected: the
// effective set would then depend on which file was last touched, so the
// same workspace would settle into different graphs on different runs.
// Scoping it to the *workspace* is a different question and is answered by
// allowedFrameworksForScope below — workspace membership is stable config,
// so it carries none of that non-determinism.
func (mi *MultiIndexer) allowedFrameworks() frameworkgate.Set {
	mi.mu.RLock()
	defer mi.mu.RUnlock()

	narrowedBy := map[string][]string{}
	out, first := frameworkgate.Set{}, true
	for prefix, idx := range mi.indexers {
		s := idx.allowedFrameworks()
		if s.Configured() {
			narrowedBy[prefix] = s.Patterns()
		}
		if first {
			out, first = s, false
			continue
		}
		out = frameworkgate.Union(out, s)
	}
	mi.logFrameworkAllowListUnion(narrowedBy, out)
	return out
}

// allowedFrameworksForScope is allowedFrameworks restricted to the WORKSPACES
// a partial synthesis run actually covers.
//
// One daemon tracks unrelated workspaces in one process, and the fold above
// unions across all of them, so a pass that only one workspace asks for still
// EXECUTES against every other workspace's scope. Measured on a 5,535-file
// Odoo reconcile in the `his` workspace: fastapi-resolve (17.9 min) and
// fn-value-callback (13.6 min) ran only because the `gortex` repository — a
// different workspace, with an honest and minimal allow-list of its own — asks
// for them. Neither contributed anything: the per-repo gate dropped every edge
// they staged (fn-value-callback staged 1,297 and lost all 1,297).
//
// This does NOT reopen the alternative rejected on allowedFrameworks. That one
// scoped the fold to the *changed prefixes*, so the effective set moved with
// whichever file happened to be touched. Workspace membership is stable
// configuration, so this set is identical on every run over the same workspace.
//
// Used by the global pass too, as of the change that added this paragraph. It
// was excluded at first on the grounds that a global run can carry a nil scope
// covering the whole store, where there is no single workspace to resolve and
// narrowing would stop emitting a sibling workspace's edges on a full derive.
// That reasoning was sound and the conclusion was too broad: the nil case is
// handled HERE, by falling back to the daemon-wide union on an empty scope or a
// scope naming no tracked repository. A global run with a NON-nil scope — every
// post-track derive, where rederiveScope returns a sibling-checkout frontier —
// has exactly one workspace to resolve and was paying the daemon-wide union for
// no reason the exclusion gave. Measured cost of that on one scoped Odoo derive:
// 178.5s across four synthesizers emitting zero edges.
//
// Note what this does NOT change: DeriveConfigHash still fingerprints the
// daemon-wide union, so a repository's recorded config hash is deliberately
// BROADER than the pass set its derive actually ran. That is the safe direction
// — it can only over-report `partial`, never under-report it — but it means an
// allow-list edit in any workspace marks every repository stale. Narrowing the
// hash to match is not a signature change: runDaemonStart stores one hash in
// runtime state, stampDeriveState stamps one for all covered repos,
// ScheduleDeriveForConfigDrift compares every repo against one current hash, and
// the CLI's applyReadiness reads one per row. Migrate all four together or leave
// it broad; narrowing it here alone would report `ready` over a stale derive.
func (mi *MultiIndexer) allowedFrameworksForScope(prefixes map[string]bool) frameworkgate.Set {
	if mi == nil {
		return frameworkgate.Set{}
	}
	if len(prefixes) == 0 {
		return mi.allowedFrameworks()
	}

	mi.mu.RLock()
	defer mi.mu.RUnlock()

	workspaces := make(map[string]bool, len(prefixes))
	for prefix := range prefixes {
		workspaces[mi.workspaceIDForPrefixLocked(prefix)] = true
	}

	out, first := frameworkgate.Set{}, true
	for prefix, idx := range mi.indexers {
		if idx == nil || !workspaces[mi.workspaceIDForPrefixLocked(prefix)] {
			continue
		}
		s := idx.allowedFrameworks()
		if first {
			out, first = s, false
			continue
		}
		out = frameworkgate.Union(out, s)
	}
	// `first` still set means the scope named no tracked repository. `out` is
	// then the unset Set, which allows everything — the pre-existing behaviour
	// and the safe direction, consistent with frameworkgate.Union.
	return out
}

// workspaceIDForPrefixLocked returns the workspace slug a repository belongs
// to, following the same singleton fallback as ReposInWorkspace: a repository
// declaring no workspace is its own workspace, keyed on its prefix. That
// fallback is what keeps one unconfigured repo from dragging siblings — it
// lands in a workspace of its own rather than widening theirs.
//
// The caller holds mi.mu.
func (mi *MultiIndexer) workspaceIDForPrefixLocked(prefix string) string {
	if idx := mi.indexers[prefix]; idx != nil {
		if ws := idx.WorkspaceID(); ws != "" {
			return ws
		}
	}
	return prefix
}

// allowedFrameworksByRepo returns each tracked repository's own
// allow-list, keyed by repository prefix. It is the enforcement half of
// the pair: allowedFrameworks decides that a pass runs, this decides
// which repositories it may write into.
//
// A repository with no list maps to the unset Set, which allows
// everything — so the map is safe to hand over unconditionally and the
// resolver skips the gate entirely when no entry is configured.
func (mi *MultiIndexer) allowedFrameworksByRepo() map[string]frameworkgate.Set {
	mi.mu.RLock()
	defer mi.mu.RUnlock()

	out := make(map[string]frameworkgate.Set, len(mi.indexers))
	for prefix, idx := range mi.indexers {
		out[prefix] = idx.allowedFrameworks()
	}
	return out
}

// logFrameworkAllowListUnion records that some repositories narrowed
// their allow-list while others did not, so the shared pass still runs
// the union. Without this the split is invisible: a user reading the
// per-pass edge counts sees a framework they excluded still executing,
// and cannot tell that it is running for a sibling repository and being
// gated out of their own.
func (mi *MultiIndexer) logFrameworkAllowListUnion(narrowedBy map[string][]string, effective frameworkgate.Set) {
	if mi.logger == nil || len(narrowedBy) == 0 || len(mi.indexers) < 2 {
		return
	}
	// Every repository narrowing to the same set is the coherent case
	// and needs no note; the union then equals each input.
	if effective.Configured() && len(narrowedBy) == len(mi.indexers) {
		return
	}
	repos := make([]string, 0, len(narrowedBy))
	for prefix := range narrowedBy {
		repos = append(repos, prefix)
	}
	sort.Strings(repos)
	mi.logger.Info("framework allow-list: shared pass runs the union; edges stay confined to the repositories that allow each pass",
		zap.Strings("narrowed_by", repos),
		zap.Int("tracked_repos", len(mi.indexers)),
		zap.String("hint", "set index.frameworks.allow in ~/.gortex/config.yaml to stop the excluded passes from running at all"))
}

// knownFrameworkNames returns every registered framework pass name across
// the three registries — route passes, dispatch synthesizers and claiming
// resolvers — for typo diagnostics and the `analyze kind=frameworks`
// inventory. This is the only place the three registries are unioned, so
// it is also the answer to "what may I put in index.frameworks.allow".
func knownFrameworkNames() []string {
	var out []string
	for _, p := range contracts.RegisteredFrameworkRoutePasses() {
		out = append(out, p.Name())
	}
	out = append(out, resolver.RegisteredFrameworkSynthesizerNames()...)
	out = append(out, resolver.RegisteredClaimingResolverNames()...)

	seen := map[string]bool{}
	deduped := out[:0]
	for _, n := range out {
		if seen[n] {
			continue
		}
		seen[n] = true
		deduped = append(deduped, n)
	}
	sort.Strings(deduped)
	return deduped
}
