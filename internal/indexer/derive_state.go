package indexer

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/zzet/gortex/internal/graph"
)

// errDeriveNoGraph reports that the global derived passes could not run at all
// because the indexer holds no graph. It is deliberately an error rather than
// a quiet zero return: the early return it names does no work and leaves
// ctx.Err() nil, so without it a caller cannot tell a no-op run from a
// complete one, and would stamp the repo as derived on the strength of
// nothing having failed.
var errDeriveNoGraph = errors.New("global graph passes: indexer has no graph")

// DerivePassVersion is the semantic version of the derived-pass tier as a
// whole. Bump it when a pass changes what it emits — a new synthesizer, a
// corrected gate, a fixed inference — so that stores derived by the previous
// build re-derive instead of reading current forever on a graph whose derived
// edges this build would no longer produce. The same contract
// repo_index_state.extractor_versions carries for extraction.
//
// Exported because the readiness reader compares against it from outside this
// package.
const DerivePassVersion = 1

const derivePassVersion = DerivePassVersion

// DeriveConfigHash fingerprints the CONFIGURATION the derived passes run
// under, so a config change invalidates a completion the way a code change
// does. Today that is the framework allow-list, which decides which of ~55
// synthesis passes execute at all — change it and the derived graph changes
// with no code change, and a stamp that only versioned code would read "ready"
// on edges the current config would never produce.
//
// The workspace-wide UNION is the right input, because that union is what
// governs execution: a pass runs when any tracked repository allows it. A
// durable lesson sits behind that — one unconfigured repository re-admits all
// ~55 passes for everyone — so the hash has to move when a sibling's list
// moves, not only when this repo's does.
//
// An unconfigured union hashes to the empty string rather than to the hash of
// nothing. Empty means "no comparison to make", and the reader skips the clause
// instead of accusing every repo of a config drift it cannot see.
// Deliberately DAEMON-WIDE, and deliberately broader than the pass set a derive
// actually runs. The global pass narrows framework execution to the covered
// workspaces (see allowedFrameworksForScope), so a repository's recorded hash
// names patterns that never executed for it, and an allow-list edit in an
// unrelated workspace marks it `partial` and re-derives it.
//
// That is the safe direction — over-reporting `partial` costs time, while
// under-reporting it serves a stale graph as complete — and it is not free to
// fix. This value has four consumers that each assume a single hash exists per
// daemon: runDaemonStart (cmd/gortex/daemon.go) stores one in runtime state,
// stampDeriveState below stamps one for ALL covered repos, ScheduleDeriveForConfigDrift
// compares every repo against one "current" hash, and applyReadiness
// (cmd/gortex/repos_cmd.go) reads one per CLI row. Scoping it is a persistence
// and runtime-state migration across all four, plus a one-time re-derive of
// every tracked repo because the digest input changes.
//
// So: migrate all four together, or leave it broad. Narrowing this function
// alone makes a repo's stamp claim a pass set that ran for someone else, which
// reports `ready` over a derive that never happened.
func (mi *MultiIndexer) DeriveConfigHash() string {
	if mi == nil {
		return ""
	}
	patterns := mi.allowedFrameworks().Patterns()
	if len(patterns) == 0 {
		return ""
	}
	// Sorted AND deduped, so the digest is a function of the allow-list as a
	// SET. The union folds over a map of tracked repositories, so its order is
	// whatever this process happened to iterate — the sort is what stops the
	// hash flipping between daemon restarts. The dedupe covers the other half:
	// N repositories allowing the same framework contribute that name N times,
	// and hashing the multiset made "track a sixth repository with an identical
	// list" look like a config change to the five already derived. Both are
	// safe to collapse because frameworkgate.Allows consults only the exact
	// map, the prefix list and the all flag — admission cannot distinguish a
	// pattern present once from the same pattern present six times, so neither
	// may the fingerprint that decides whether a derive is still current.
	// frameworkgate.New now dedupes too; this stays explicit because the
	// stability of a stamped hash must not rest on a leaf package's internals.
	sorted := make([]string, 0, len(patterns))
	seen := make(map[string]bool, len(patterns))
	for _, p := range patterns {
		lowered := strings.ToLower(strings.TrimSpace(p))
		if lowered == "" || seen[lowered] {
			continue
		}
		seen[lowered] = true
		sorted = append(sorted, lowered)
	}
	if len(sorted) == 0 {
		return ""
	}
	sort.Strings(sorted)
	// NUL-joined: a pattern cannot contain one, so no two distinct lists can
	// collide by concatenation ("a","bc" vs "ab","c").
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return hex.EncodeToString(sum[:8])
}

// derivedCoverage lists the repos a global-pass run covers.
//
// It keys off fullCoverage, not off scope being nil, because those are not the
// same question. A batch can arm an explicit all-repository scope and still run
// whole-graph — the daemon's warm restart does exactly that, carrying a
// detached census attestation so the passes scan the raw store while the scope
// survives for per-repository state. Reading only `scope != nil` there would
// claim just the changed repos for a run that genuinely derived every one, and
// a repo that has never been stamped would stay unknown until some later run
// happened to arrive with a nil scope.
//
// A genuinely scoped run covers exactly its frontier. Such a run can still land
// a cross-repo edge whose far endpoint lies outside that frontier, which
// advances the far repo's anchor without advancing its derive_state — so it
// reads partial until the next whole-graph derive clears it. That is the
// conservative direction on purpose. Widening coverage to "every repo whose
// generation this run moved" would claim a repo derived because an edge
// happened to land in it, and report ready for one whose own passes may never
// have run at all.
func (mi *MultiIndexer) derivedCoverage(scope map[string]struct{}, fullCoverage bool) []string {
	if scope != nil && !fullCoverage {
		out := make([]string, 0, len(scope))
		for prefix := range scope {
			if prefix != "" {
				out = append(out, prefix)
			}
		}
		sort.Strings(out)
		return out
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	out := make([]string, 0, len(mi.indexers))
	for prefix := range mi.indexers {
		if prefix != "" {
			out = append(out, prefix)
		}
	}
	sort.Strings(out)
	return out
}

// completeDerive records a finished global-pass run, or records why it did not
// finish. Nothing is stamped for a preempted or failed run: derive_state
// asserts "these repos are derived", and a run that returned from the middle
// derived some unknown prefix of its work. Leaving the row alone makes that
// repo read partial, which is true, until a later run completes.
func (mi *MultiIndexer) completeDerive(covered []string, scoped bool, err error) {
	if err != nil {
		if mi.logger != nil {
			mi.logger.Info("global passes did not complete; derive state not stamped",
				zap.Bool("scoped", scoped),
				zap.Int("would_have_covered", len(covered)),
				zap.Error(err))
		}
		return
	}
	mi.stampDeriveState(covered, scoped)
}

// stampDeriveState records a completed derive for every repo the run covered.
//
// The graph generation is not passed in — StampDeriveState reads each repo's
// current one inside the transaction that writes the row. That is not a
// detail: the derived passes emit edges themselves, so their own writes
// advance the anchor while they run, and any generation read before the last
// pass committed is already behind. See store_sqlite.StampDeriveState.
//
// derived_sha comes from repo_index_state rather than a fresh rev-parse. It is
// provenance that is never compared, and the indexed SHA is the more truthful
// value anyway: it is the revision the passes actually read, where HEAD may
// have moved since. It also keeps this off the git subprocess path, which the
// incremental derive walks on every file save.
func (mi *MultiIndexer) stampDeriveState(covered []string, scoped bool) {
	if mi == nil || len(covered) == 0 || mi.graph == nil {
		return
	}
	w, ok := graph.Store(mi.graph).(graph.DeriveStateStore)
	if !ok {
		// The in-memory graph persists no per-repo state, exactly as it
		// persists no index state or file-mtime ledger.
		return
	}
	reader, _ := graph.Store(mi.graph).(graph.RepoIndexStateReader)
	// Hashed once for the whole run: the allow-list is a workspace-wide union,
	// so every repo this run covered was derived under the same one, and the
	// fold walks every tracked indexer.
	configHash := mi.DeriveConfigHash()
	completions := make([]graph.DeriveCompletion, 0, len(covered))
	for _, prefix := range covered {
		c := graph.DeriveCompletion{
			RepoPrefix:  prefix,
			PassVersion: derivePassVersion,
			ConfigHash:  configHash,
			Scoped:      scoped,
		}
		if reader != nil {
			if st, found, err := reader.GetRepoIndexState(prefix); err == nil && found {
				c.DerivedSHA = st.IndexedSHA
			}
		}
		completions = append(completions, c)
	}
	if err := w.StampDeriveState(completions, time.Now().Unix()); err != nil {
		if mi.logger != nil {
			mi.logger.Warn("persist derive state failed",
				zap.Int("repos", len(completions)),
				zap.Bool("scoped", scoped),
				zap.Error(err))
		}
	}
}

// ScheduleDeriveForConfigDrift schedules one workspace derivation when the
// derive-relevant configuration has moved since the tracked repositories were
// last stamped, and returns the repositories that were behind.
//
// Nothing else closes this gap. DeriveConfigHash is published to the runtime
// state and stamped onto each completion, and readiness compares the two — but
// no caller consumed the comparison to schedule work. So a config change put
// every repository into "partial: derive-relevant config changed" with no path
// back to ready, and the remedy the CLI prints under that very message did not
// help: warmup's change detection is file-based, so a restart with no file
// delta takes the ResetBatch fast path and runs no workspace-wide pass at all.
// Observed 2026-08-30 — five repositories sat partial from 18:51 through a
// 22:59 restart and would have survived every future one.
//
// Deliberately NOT folded into warmup's anyChanged. That flag also drives the
// resolve scope, the enrichment overlap and the batch transition; forcing it
// true with an empty changed set would push a whole-workspace derive down a
// path built for a file delta. Scheduling instead reuses the post-track
// scheduler, which is preemptible, publishes its owed set, and already knows
// how to run a whole-workspace pass.
//
// The calls coalesce: the scheduler's debounce is what makes a burst of tracks
// one pass, and this is the same burst shape, so N repositories cost one
// derivation rather than N.
func (mi *MultiIndexer) ScheduleDeriveForConfigDrift() []string {
	if mi == nil || mi.graph == nil {
		return nil
	}
	current := mi.DeriveConfigHash()
	if current == "" {
		// An unconfigured union has no comparison to make — the same reason
		// the readiness reader skips its config clause rather than guessing.
		return nil
	}
	store, ok := graph.Store(mi.graph).(graph.DeriveStateStore)
	if !ok {
		return nil
	}

	mi.mu.RLock()
	prefixes := make([]string, 0, len(mi.repos))
	for prefix := range mi.repos {
		prefixes = append(prefixes, prefix)
	}
	mi.mu.RUnlock()
	sort.Strings(prefixes)

	var stale []string
	for _, prefix := range prefixes {
		st, found, err := store.GetDeriveState(prefix)
		if err != nil || !found || st.Legacy {
			// Never derived, or derived before completion was recorded.
			// Readiness reports both on their own terms and neither is a
			// config drift, so neither is this function's to diagnose —
			// claiming them here would hide a genuinely underived repo behind
			// a config message.
			continue
		}
		if st.ConfigHash != current {
			stale = append(stale, prefix)
		}
	}
	for _, prefix := range stale {
		mi.scheduleWorkspaceRederive(prefix)
	}
	return stale
}

// intersectPrefixes keeps only the prefixes present in both sets, preserving
// the first argument's order. It exists so a caller can narrow a whole-graph
// coverage claim to the repos it actually holds a mutation lane for.
func intersectPrefixes(covered, allowed []string) []string {
	if len(covered) == 0 || len(allowed) == 0 {
		return nil
	}
	keep := make(map[string]struct{}, len(allowed))
	for _, prefix := range allowed {
		keep[prefix] = struct{}{}
	}
	out := make([]string, 0, len(covered))
	for _, prefix := range covered {
		if _, ok := keep[prefix]; ok {
			out = append(out, prefix)
		}
	}
	return out
}

// sortedPrefixes flattens a prefix set into a stable slice, dropping the
// unprefixed single-repo sentinel that has no per-repo state row.
func sortedPrefixes(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for prefix := range set {
		if prefix != "" {
			out = append(out, prefix)
		}
	}
	sort.Strings(out)
	return out
}

// refreshDeriveState renews an existing completion for each named repo without
// creating one. See store_sqlite.RefreshDeriveState: the incremental derived
// passes re-derive only the families an edit invalidated, so they can keep a
// derived repo current but must never claim a repo the global passes have
// never covered.
func (mi *MultiIndexer) refreshDeriveState(prefixes []string) {
	if mi == nil || len(prefixes) == 0 || mi.graph == nil {
		return
	}
	w, ok := graph.Store(mi.graph).(graph.DeriveStateStore)
	if !ok {
		return
	}
	if _, err := w.RefreshDeriveState(prefixes, time.Now().Unix()); err != nil && mi.logger != nil {
		mi.logger.Warn("refresh derive state failed",
			zap.Int("repos", len(prefixes)), zap.Error(err))
	}
}

// RuntimeMarker publishes long-running per-repo work to whatever surface an
// out-of-band reader can see. The daemon installs one that writes its runtime
// state file; every other caller leaves it nil.
//
// The indirection exists because internal/daemon imports this package, so the
// publication cannot go the other way. It is also why the marker is an
// interface rather than a direct call: the indexer must not know or care that
// the destination happens to be a file.
type RuntimeMarker interface {
	// DeriveBegan opens a derived-pass run over exactly the named repos. An
	// empty scope means the whole workspace.
	DeriveBegan(scope []string, configHash string)
	// DeriveEnded closes the run, whether it completed or was preempted. A
	// marker left open would freeze a reader on "deriving…" until the daemon
	// exits, so this must fire on every exit path.
	DeriveEnded()
	// DerivePendingChanged republishes the set of repos a derived-pass run is
	// owed to but has not opened yet — queued behind the debounce, behind the
	// cross-repo resolve, or behind the batch-mutation gate. Level-triggered
	// for the same reason as EnrichingChanged: each call carries the complete
	// current set, so a dropped publication is repaired by the next transition
	// instead of stranding a reader on a set that no longer exists.
	DerivePendingChanged(prefixes []string)
	// EnrichingChanged republishes the set of repos with an enrichment pass in
	// flight. It is level-triggered, not edge-triggered: each call carries the
	// complete current set, so a dropped notification is repaired by the next
	// one rather than leaving the reader permanently wrong.
	EnrichingChanged(repos []string)
}

// SetRuntimeMarker installs the publisher for in-flight derive and enrichment
// work, and links it to the semantic manager if one is already installed.
//
// The daemon sets the marker and the semantic manager in whichever order its
// startup happens to take, so both installers re-establish the link rather
// than assuming the other went first.
func (mi *MultiIndexer) SetRuntimeMarker(marker RuntimeMarker) {
	mi.mu.Lock()
	mi.runtimeMarker = marker
	sem := mi.semanticMgr
	mi.mu.Unlock()
	if sem != nil && marker != nil {
		sem.SetActivityHook(marker.EnrichingChanged)
	}
}

func (mi *MultiIndexer) runtimeMarkerRef() RuntimeMarker {
	if mi == nil {
		return nil
	}
	mi.mu.RLock()
	defer mi.mu.RUnlock()
	return mi.runtimeMarker
}
