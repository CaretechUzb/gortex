package analyzer_test

// PURPOSE — the census must never present a truncated scan as a complete
// answer. AnalyzeSynthesizers returning an empty rollup with no error was the
// whole defect: on a large store an aborted read is indistinguishable from a
// graph in which no synthesizer ever fired.
// KEYWORDS — synthesizers, silent-failure, abort

import (
	"errors"
	"iter"
	"testing"

	"github.com/zzet/gortex/internal/analyzer"
	"github.com/zzet/gortex/internal/graph"
)

func mustAnalyzeSynthesizers(t *testing.T, g graph.Store, opts ...analyzer.SynthesizersOption) analyzer.SynthesizersResult {
	t.Helper()
	res, err := analyzer.AnalyzeSynthesizers(g, opts...)
	if err != nil {
		t.Fatalf("AnalyzeSynthesizers: %v", err)
	}
	return res
}

// abortingStore yields real rows and then reports that the scan was cut short,
// which is exactly what a store swap during a graph hot-reload produces. The
// embedded nil Store is deliberate: nothing but the sequencer may be called,
// and a panic is a better failure than a silently wrong fallback.
type abortingStore struct {
	graph.Store
	rows int
}

func (a abortingStore) SynthesizedEdgesSeq() (iter.Seq[graph.SynthesizedEdge], func() error) {
	return func(yield func(graph.SynthesizedEdge) bool) {
			for i := 0; i < a.rows; i++ {
				if !yield(graph.SynthesizedEdge{
					From: "repo/a.go::A", To: "repo/b.go::B",
					Kind: graph.EdgeReferences, SynthesizedBy: "odoo",
				}) {
					return
				}
			}
		}, func() error {
			return errors.New("statement is closed")
		}
}

func TestAnalyzeSynthesizers_AbortedScanIsAnErrorNotAnEmptyCensus(t *testing.T) {
	res, err := analyzer.AnalyzeSynthesizers(abortingStore{})
	if err == nil {
		t.Fatal("an aborted scan must not be reported as a successful empty census")
	}
	// The partial tally must not travel with the error: a caller that logs the
	// error and renders the result anyway would publish a wrong total.
	if res.TotalEdges != 0 || len(res.Synthesizers) != 0 {
		t.Fatalf("an aborted scan must yield no partial result, got %+v", res)
	}
}

// The counterpart that keeps the error meaningful: a scan that really did see
// nothing is NOT an error, so an empty census stays distinguishable from a
// broken one.
func TestAnalyzeSynthesizers_EmptyGraphIsNotAnError(t *testing.T) {
	res := mustAnalyzeSynthesizers(t, newTestGraph())
	if res.TotalEdges != 0 || len(res.Synthesizers) != 0 {
		t.Fatalf("expected an empty census, got %+v", res)
	}
}

// A partial scan that aborts after yielding rows is the dangerous shape: the
// tally looks plausible, so only the error distinguishes it.
func TestAnalyzeSynthesizers_PartialScanIsRejected(t *testing.T) {
	if _, err := analyzer.AnalyzeSynthesizers(abortingStore{rows: 3}); err == nil {
		t.Fatal("a scan that yielded rows and then aborted must still be an error")
	}
}
