// Package readiness answers one question about a repository: can a query
// against it be trusted to return everything it should?
//
// It lives in its own package because two surfaces need that answer and must
// not drift. `gortex repos` renders it as a column; the MCP server attaches it
// to a tool result so an answer can say when it is incomplete. A verdict
// computed in two places is a verdict that disagrees with itself eventually.
//
// Everything here is pure — no git, no sqlite, no daemon — which is what makes
// the ladder below reachable from a struct literal, one case per rung.
package readiness

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
//	                   ┌──────────────────────────────────────┐
//	                   │  for each repo in the global config   │
//	                   └──────────────────┬───────────────────┘
//	                                      v
//	path is gone ─────────────yes───────> MISSING        no checkout to be ready
//	                           │no
//	no repo_index_state row ───yes───────> not indexed
//	                           │no
//	indexed sha != git HEAD ───yes───────> stale         the index is behind
//	                           │no
//	a live daemon is working ──yes───────> deriving… /   in flight or queued,
//	or owes work on this repo  │           enriching…    not broken
//	                           │no
//	derive_state table absent ─yes───────> unknown       binary newer than store
//	                           │no
//	derive row is a v13 legacy ─yes──────> unknown       never actually recorded
//	                           │no
//	no derive row at all ──────yes───────> never derived THE bug: warmup-swallowed
//	                           │no
//	derived_content_gen <      ─yes──────> partial       content moved since
//	  repo content_gen         │
//	OR pass_version stale      │                         synthesis changed
//	OR config_hash differs     │                         allow-list changed
//	                           │no
//	enrichment verdict stale ──yes───────> partial       a provider is behind
//	                           │
//	                           v
//	                         ready
//
// Every state is reachable from a struct literal, which is the point of keeping
// this pure: no git, no sqlite, no daemon.
const (
	LabelReady        = "ready"
	LabelPartial      = "partial"
	LabelNeverDerived = "never derived"
	LabelUnknown      = "unknown"
	LabelDeriving     = "deriving…"
	LabelEnriching    = "enriching…"
	LabelStale        = "stale"
	LabelNotIndexed   = "not indexed"
	LabelMissing      = "MISSING"
)

// Enrichment sub-verdicts, reported in --json as `enriched`.
const (
	EnrichLabelCurrent = "current"
	EnrichLabelStale   = "stale"
	EnrichLabelNA      = "n/a"
	EnrichLabelUnknown = "unknown"
)

// RepoState is the per-repo checkout facts the verdict needs, projected out of
// whatever row the calling surface already holds.
//
// It exists so this package does not depend on the CLI's table row. The ladder
// reads exactly these four fields; taking the whole row instead would drag a
// presentation type into the one place that has to stay shared.
type RepoState struct {
	// Missing is a checkout that is gone from disk.
	Missing bool
	// Indexed is false when there is no repo_index_state row at all.
	Indexed bool
	// Stale is an index behind git HEAD.
	Stale bool
	// Path is the checkout path, used only to build a remediation command.
	Path string
}

// Inputs is everything Verdict needs that is not already on the RepoState: the
// store's raw rows, the two live-work markers, and the values the running build
// would stamp today.
type Inputs struct {
	// DeriveTable and EnrichTable report whether each table exists at all — the
	// window where this binary reads a store an older daemon wrote and has not
	// migrated. Absent is "unknown", not "never derived": accusing every repo of
	// a missing derive because the reader is ahead of the writer would be a
	// false alarm on every upgrade.
	DeriveTable bool
	EnrichTable bool

	Repo store_sqlite.RepoReadiness

	// Deriving, DerivePending and Enriching come from the daemon's runtime
	// record, already filtered to this repo and already discarded wholesale if
	// the recording process is dead.
	//
	// DerivePending is the run the scheduler owes but has not opened. It is
	// separate from Deriving because the two are separated by minutes — a
	// debounce, a checkout-group republish, a whole-workspace cross-repo
	// resolve and the batch-mutation gate all sit between them — and a repo
	// tracked into that gap has no derive_state row, so without it the verdict
	// falls through to "never derived" while the daemon is working on exactly
	// that repo.
	Deriving      bool
	DerivePending bool
	Enriching     bool

	// PassVersion is what this build's derived passes would stamp. ConfigHash is
	// what the running daemon's derive-relevant config hashes to, or empty when
	// no daemon is up — in which case there is nothing to compare against and
	// the config clause is skipped rather than guessed at.
	PassVersion int64
	ConfigHash  string
}

// EnrichVerdict reduces a repo's provider rows to one word.
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
func EnrichVerdict(in Inputs) string {
	if !in.EnrichTable || in.Repo.EnrichRows == 0 {
		return EnrichLabelUnknown
	}
	if in.Repo.EnrichProviders == 0 {
		// Either the sentinel, or only the whole-repo rollup marker — neither
		// is a provider, so nothing applicable is outstanding.
		return EnrichLabelNA
	}
	if in.Repo.EnrichMinContentGen < in.Repo.ContentGen {
		return EnrichLabelStale
	}
	return EnrichLabelCurrent
}

// Verdict returns the composite label and, when it is not "ready", a short
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
func Verdict(entry RepoState, in Inputs) (label, reason string) {
	switch {
	case entry.Missing:
		return LabelMissing, "the checkout is gone; run gortex untrack " + entry.Path
	case !entry.Indexed:
		return LabelNotIndexed, "never indexed; run gortex track " + entry.Path
	case entry.Stale:
		return LabelStale, "the index is behind git HEAD; the daemon reindexes on its own"
	case in.Deriving:
		return LabelDeriving, "derived passes are running now"
	case in.DerivePending:
		// Same label, different reason. A reader needs to know the repo is
		// being worked on, which is the label's whole job; whether the pass
		// has opened yet is detail, and a separate label would ripple through
		// readyCell, the summary buckets and the JSON contract for no gain.
		return LabelDeriving, "derived passes are queued for this repo"
	case in.Enriching:
		return LabelEnriching, "semantic enrichment is running now"
	case !in.DeriveTable:
		return LabelUnknown, "this store predates derive tracking; restart the daemon to migrate it"
	case !in.Repo.DeriveFound:
		return LabelNeverDerived,
			"the derived passes have never run for this repo, so cross-repo and interface queries return a subset; restart the daemon"
	case in.Repo.Derive.Legacy:
		return LabelUnknown, "derived before completion was recorded; the next derive settles it"
	case in.Repo.Derive.DerivedContentGen < in.Repo.ContentGen:
		return LabelPartial, "files changed since the derived passes last ran"
	case in.PassVersion > 0 && in.Repo.Derive.PassVersion < in.PassVersion:
		return LabelPartial, "derived by an older synthesis version; a re-derive refreshes it"
	case in.ConfigHash != "" && in.Repo.Derive.ConfigHash != in.ConfigHash:
		return LabelPartial, "derive-relevant config changed since the passes last ran"
	case EnrichVerdict(in) == EnrichLabelStale:
		return LabelPartial, "semantic enrichment is behind the indexed content"
	}
	return LabelReady, ""
}

// BlocksQueries reports whether a verdict means a query against this repo may
// quietly return less than it should. It drives the CLI's stderr remediation
// hint and the MCP readiness note: a column that names a problem and no action
// is half a feature, and listing every non-ready repo would bury the two states
// a user can actually do something about under the ones that resolve
// themselves.
//
// The narrowness is the design, not a gap. `deriving…` and `stale` are being
// fixed by the daemon as they are read, so warning about them trains the reader
// to ignore the warning — which costs exactly the two states that matter.
func BlocksQueries(label string) bool {
	return label == LabelNeverDerived || label == LabelPartial
}
