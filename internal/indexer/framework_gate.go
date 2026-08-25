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
