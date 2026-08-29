package main

import (
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

// The composite READY verdict, in strict priority order.
//
// FRESHNESS answers "is the index current with git HEAD?", which is only the
// first of three per-repo stages. Afterwards the daemon runs the derived passes
// (implements, overrides, test edges, entrypoint hierarchy, capability edges,
// framework-dispatch synthesis, external-call placeholders, cross-repo edges)
// and, where the languages qualify, semantic enrichment. Until those finish,
// "who uses this" returns a subset — silently, with nothing in the output
// saying so. READY is that missing signal.
//
//	                    ┌──────────────────────────────────────┐
//	                    │  for each repo in the global config   │
//	                    └──────────────────┬───────────────────┘
//	                                       v
//	 path is gone ─────────────yes───────> MISSING        no checkout to be ready
//	                            │no
//	 no repo_index_state row ───yes───────> not indexed
//	                            │no
//	 indexed sha != git HEAD ───yes───────> stale         the index is behind
//	                            │no
//	 a live daemon is working ──yes───────> deriving… /   in flight, not broken
//	 on this repo               │           enriching…
//	                            │no
//	 derive_state table absent ─yes───────> unknown       binary newer than store
//	                            │no
//	 derive row is a v13 legacy ─yes──────> unknown       never actually recorded
//	                            │no
//	 no derive row at all ──────yes───────> never derived THE bug: warmup-swallowed
//	                            │no
//	 derived_content_gen <      ─yes──────> partial       content moved since
//	   repo content_gen         │
//	 OR pass_version stale      │                         synthesis changed
//	 OR config_hash differs     │                         allow-list changed
//	                            │no
//	 enrichment verdict stale ──yes───────> partial       a provider is behind
//	                            │
//	                            v
//	                          ready
//
// Every state is reachable from a struct literal, which is the point of keeping
// this pure: no git, no sqlite, no daemon.
const (
	readyLabelReady        = "ready"
	readyLabelPartial      = "partial"
	readyLabelNeverDerived = "never derived"
	readyLabelUnknown      = "unknown"
	readyLabelDeriving     = "deriving…"
	readyLabelEnriching    = "enriching…"
	readyLabelStale        = "stale"
	readyLabelNotIndexed   = "not indexed"
	readyLabelMissing      = "MISSING"
)

// Enrichment sub-verdicts, reported in --json as `enriched`.
const (
	enrichLabelCurrent = "current"
	enrichLabelStale   = "stale"
	enrichLabelNA      = "n/a"
	enrichLabelUnknown = "unknown"
)

// readinessInputs is everything readyVerdict needs that is not already on the
// repoStatus: the store's raw rows, the two live-work markers, and the values
// the running build would stamp today.
type readinessInputs struct {
	// deriveTable and enrichTable report whether each table exists at all — the
	// window where this binary reads a store an older daemon wrote and has not
	// migrated. Absent is "unknown", not "never derived": accusing every repo of
	// a missing derive because the reader is ahead of the writer would be a
	// false alarm on every upgrade.
	deriveTable bool
	enrichTable bool

	repo store_sqlite.RepoReadiness

	// deriving and enriching come from the daemon's runtime record, already
	// filtered to this repo and already discarded wholesale if the recording
	// process is dead.
	deriving  bool
	enriching bool

	// passVersion is what this build's derived passes would stamp. configHash is
	// what the running daemon's derive-relevant config hashes to, or empty when
	// no daemon is up — in which case there is nothing to compare against and
	// the config clause is skipped rather than guessed at.
	passVersion int64
	configHash  string
}

// enrichVerdict reduces a repo's provider rows to one word.
//
// The reduction is a MINIMUM over the applicable providers, never a maximum and
// never the newest. A repo with python-types current and go-types never run is
// not enriched, and any aggregate that lets the fresh one speak for the set
// reports it as though it were.
//
// The two absences mean different things. The __none__ sentinel is the
// enrichment pass having looked and found nothing applicable — a real answer,
// and one that never blocks ready. No rows at all is nobody having looked, and
// is reported as unknown.
func enrichVerdict(in readinessInputs) string {
	if !in.enrichTable || in.repo.EnrichRows == 0 {
		return enrichLabelUnknown
	}
	if in.repo.EnrichProviders == 0 {
		// Either the sentinel, or only the whole-repo rollup marker — neither
		// is a provider, so nothing applicable is outstanding.
		return enrichLabelNA
	}
	if in.repo.EnrichMinContentGen < in.repo.ContentGen {
		return enrichLabelStale
	}
	return enrichLabelCurrent
}

// readyVerdict returns the composite label and, when it is not "ready", a short
// reason naming what to do about it.
//
// An enrichment verdict of "unknown" deliberately does NOT block ready. It is
// an absence of evidence, not evidence of absence, and the states that produce
// it — a store an older daemon wrote, a repo the enrichment pass has not
// reached — are already reported by the derive clauses above it or by the
// `enriched` field in --json. Treating it as a failure would mark every repo on
// a pre-upgrade store as not ready, which is a false alarm the user cannot act
// on. The residue this leaves uncaught is a repo that was derived but whose
// enrichment pass never ran at all; it reads ready with `enriched: unknown`.
func readyVerdict(entry repoStatus, in readinessInputs) (label, reason string) {
	switch {
	case entry.Missing:
		return readyLabelMissing, "the checkout is gone; run gortex untrack " + entry.Path
	case !entry.Indexed:
		return readyLabelNotIndexed, "never indexed; run gortex track " + entry.Path
	case entry.Stale:
		return readyLabelStale, "the index is behind git HEAD; the daemon reindexes on its own"
	case in.deriving:
		return readyLabelDeriving, "derived passes are running now"
	case in.enriching:
		return readyLabelEnriching, "semantic enrichment is running now"
	case !in.deriveTable:
		return readyLabelUnknown, "this store predates derive tracking; restart the daemon to migrate it"
	case !in.repo.DeriveFound:
		return readyLabelNeverDerived,
			"the derived passes have never run for this repo, so cross-repo and interface queries return a subset; restart the daemon"
	case in.repo.Derive.Legacy:
		return readyLabelUnknown, "derived before completion was recorded; the next derive settles it"
	case in.repo.Derive.DerivedContentGen < in.repo.ContentGen:
		return readyLabelPartial, "files changed since the derived passes last ran"
	case in.passVersion > 0 && in.repo.Derive.PassVersion < in.passVersion:
		return readyLabelPartial, "derived by an older synthesis version; a re-derive refreshes it"
	case in.configHash != "" && in.repo.Derive.ConfigHash != in.configHash:
		return readyLabelPartial, "derive-relevant config changed since the passes last ran"
	case enrichVerdict(in) == enrichLabelStale:
		return readyLabelPartial, "semantic enrichment is behind the indexed content"
	}
	return readyLabelReady, ""
}

// readyBlocksQueries reports whether a verdict means a query against this repo
// may quietly return less than it should. It drives the stderr remediation
// hint: a column that names a problem and no action is half a feature, and
// listing every non-ready repo would bury the two states a user can actually
// do something about under the ones that resolve themselves.
func readyBlocksQueries(label string) bool {
	return label == readyLabelNeverDerived || label == readyLabelPartial
}
