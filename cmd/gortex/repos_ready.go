package main

import (
	"github.com/zzet/gortex/internal/readiness"
)

// The verdict ladder itself lives in internal/readiness.
//
// It moved there because `gortex repos` is no longer its only consumer: the MCP
// server attaches the same verdict to a tool result, so an answer can say when
// it is incomplete. package main is importable from nowhere, so the only way to
// share it was to move it — and a second implementation beside this one would
// disagree with it eventually, which is worse than either.
//
// What stays here is the CLI's vocabulary. The table, JSON and summary code
// already spell these labels in local terms; aliasing rather than rewriting
// keeps that code, and its tests, reading exactly as before.
const (
	readyLabelReady        = readiness.LabelReady
	readyLabelPartial      = readiness.LabelPartial
	readyLabelNeverDerived = readiness.LabelNeverDerived
	readyLabelUnknown      = readiness.LabelUnknown
	readyLabelDeriving     = readiness.LabelDeriving
	readyLabelEnriching    = readiness.LabelEnriching
	readyLabelStale        = readiness.LabelStale
	readyLabelNotIndexed   = readiness.LabelNotIndexed
	readyLabelMissing      = readiness.LabelMissing
)

// Enrichment sub-verdicts, reported in --json as `enriched`.
const (
	enrichLabelCurrent = readiness.EnrichLabelCurrent
	enrichLabelStale   = readiness.EnrichLabelStale
	enrichLabelNA      = readiness.EnrichLabelNA
	enrichLabelUnknown = readiness.EnrichLabelUnknown
)

// readinessInputs is the CLI's spelling of readiness.Inputs. An alias, not a
// wrapper: applyReadiness fills it and hands it straight through, and a
// conversion layer between two identical structs is a place for a field to be
// forgotten.
type readinessInputs = readiness.Inputs

// readyVerdict projects the CLI's table row onto readiness.RepoState and
// defers to the shared ladder.
//
// The projection is the only thing this file contributes. readiness takes four
// checkout facts rather than the whole repoStatus so the shared package does
// not depend on a presentation type that exists to be rendered as a table.
func readyVerdict(entry repoStatus, in readinessInputs) (label, reason string) {
	return readiness.Verdict(readiness.RepoState{
		Missing: entry.Missing,
		Indexed: entry.Indexed,
		Stale:   entry.Stale,
		Path:    entry.Path,
	}, in)
}

// enrichVerdict reduces a repo's provider rows to one word. See
// readiness.EnrichVerdict for why the reduction is a minimum.
func enrichVerdict(in readinessInputs) string { return readiness.EnrichVerdict(in) }

// readyBlocksQueries reports whether a verdict means a query against this repo
// may quietly return less than it should. It drives the stderr remediation
// hint here, and the readiness note on the MCP surface.
func readyBlocksQueries(label string) bool { return readiness.BlocksQueries(label) }
