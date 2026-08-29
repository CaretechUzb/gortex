package indexer

import (
	"errors"
	"sort"
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

// derivePassVersion is the semantic version of the derived-pass tier as a
// whole. Bump it when a pass changes what it emits — a new synthesizer, a
// corrected gate, a fixed inference — so that stores derived by the previous
// build re-derive instead of reading current forever on a graph whose derived
// edges this build would no longer produce.
const derivePassVersion = 1

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
	completions := make([]graph.DeriveCompletion, 0, len(covered))
	for _, prefix := range covered {
		c := graph.DeriveCompletion{
			RepoPrefix:  prefix,
			PassVersion: derivePassVersion,
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
	DeriveBegan(scope []string)
	// DeriveEnded closes the run, whether it completed or was preempted. A
	// marker left open would freeze a reader on "deriving…" until the daemon
	// exits, so this must fire on every exit path.
	DeriveEnded()
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
