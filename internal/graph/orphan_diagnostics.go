package graph

import (
	"math"
	"strconv"
	"strings"
)

// Path-liveness diagnostics.
//
// Every freshness signal the daemon publishes is derived from the files it
// already tracks: the mtime ledger says a file changed, the git watcher says a
// commit touched it, the reconcile census says it vanished. All three are
// keyed by the tracked-file set, so none of them can see a node whose file
// left the disk WITHOUT the daemon recording the departure — a directory
// deleted while the daemon was down, an mtime record pruned without its nodes,
// a repo whose checkout was removed under a still-registered prefix.
//
// Those nodes keep answering searches with code that no longer exists, and
// nothing in index_health contradicts them: the parse ratio is about files
// that WERE indexed, and staleness is about files still in the ledger. The
// only way to see this population is to ask the filesystem about the paths the
// graph itself claims, which is what this audit does.
//
// It is deliberately a sample: the scan is bounded so a liveness probe on a
// half-million-file workspace stays a probe. A rate over a large sample is
// enough to tell "clean" from "a third of this graph is dead".

// FilePathState is one path's verdict in the liveness audit.
type FilePathState int

const (
	// FilePathLive means the indexed path still resolves to a file on disk.
	FilePathLive FilePathState = iota
	// FilePathGone means the path is absent from disk: the graph is holding
	// nodes for a file that no longer exists.
	FilePathGone
	// FilePathUnknown means liveness could not be established — the repo root
	// for the node's prefix is not resolvable, or the stat failed for a reason
	// other than absence. Counted separately and never treated as an orphan:
	// a permission error is not evidence of a deleted file.
	FilePathUnknown
)

// OrphanDiagnostics is a bounded path-liveness audit of the graph's indexed
// files.
type OrphanDiagnostics struct {
	// Checked is the number of paths whose liveness was actually decided
	// (live + gone). Unresolvable counts the rest.
	Checked int
	// Orphans is how many of the checked paths are absent from disk.
	Orphans int
	// Unresolvable is how many candidate paths could not be decided.
	Unresolvable int

	// OrphanSamples holds up to sampleLimit orphan node IDs so a
	// recommendation can name offenders rather than only count them.
	OrphanSamples []string
	// OrphansByRepo attributes orphans to the repo prefix that owns them,
	// which is what tells a whole-repo ghost apart from a deleted subtree.
	OrphansByRepo map[string]int

	// Candidates is the size of the population the audit could have drawn
	// from. When it exceeds Checked+Unresolvable the scan stopped at its cap
	// and Truncated is set, making the counts a sample rather than a census.
	Candidates int
	Truncated  bool
}

// Record folds one decided path into the audit.
func (d *OrphanDiagnostics) Record(nodeID, repoPrefix string, state FilePathState, sampleLimit int) {
	switch state {
	case FilePathUnknown:
		d.Unresolvable++
	case FilePathGone:
		d.Checked++
		d.Orphans++
		if len(d.OrphanSamples) < sampleLimit {
			d.OrphanSamples = append(d.OrphanSamples, nodeID)
		}
		if d.OrphansByRepo == nil {
			d.OrphansByRepo = map[string]int{}
		}
		d.OrphansByRepo[repoPrefix]++
	default:
		d.Checked++
	}
}

// Clean reports whether every decided path still exists on disk. An audit that
// decided nothing is clean by default — absence of evidence is not a defect.
func (d OrphanDiagnostics) Clean() bool { return d.Orphans == 0 }

// Rate is the fraction of decided paths that are orphans, in [0,1].
func (d OrphanDiagnostics) Rate() float64 {
	if d.Checked <= 0 {
		return 0
	}
	return float64(d.Orphans) / float64(d.Checked)
}

// LiveScore is Rate expressed as a health percentage rounded to one decimal,
// so it composes with the parse-success score index_health already reports.
func (d OrphanDiagnostics) LiveScore() float64 {
	if d.Checked <= 0 {
		return 100
	}
	return math.Round((1-d.Rate())*1000) / 10
}

// EstimatedOrphans extrapolates the observed rate over the whole candidate
// population. It equals Orphans when the scan was a census; on a truncated
// scan it is indicative rather than exact, since the audit sees whatever
// prefix of the walk fit under the cap rather than a uniform sample.
func (d OrphanDiagnostics) EstimatedOrphans() int {
	if !d.Truncated || d.Checked <= 0 {
		return d.Orphans
	}
	return int(math.Round(d.Rate() * float64(d.Candidates)))
}

// Summary renders the audit as a short clause for a log line or a
// recommendation string.
func (d OrphanDiagnostics) Summary() string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(d.Orphans))
	b.WriteString(" of ")
	b.WriteString(strconv.Itoa(d.Checked))
	b.WriteString(" indexed files no longer exist on disk")
	if d.Truncated {
		b.WriteString(" (sampled from ")
		b.WriteString(strconv.Itoa(d.Candidates))
		b.WriteString(", ~")
		b.WriteString(strconv.Itoa(d.EstimatedOrphans()))
		b.WriteString(" total)")
	}
	if len(d.OrphanSamples) > 0 {
		b.WriteString(", e.g. ")
		b.WriteString(strings.Join(d.OrphanSamples, ", "))
	}
	return b.String()
}
