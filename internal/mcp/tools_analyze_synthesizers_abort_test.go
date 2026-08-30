package mcp

// PURPOSE — the handler must not launder an aborted census into a normal
// answer. This is the surface the user actually touches: `gortex analyze --kind
// synthesizers` reaches handleAnalyzeSynthesizers through handleAnalyze, so a
// swallowed error here is the whole defect regardless of what the analyzer
// returns.
// KEYWORDS — synthesizers, silent-failure, abort, handler

import (
	"context"
	"errors"
	"iter"
	"testing"

	mcplib "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// abortingCensusStore reports a scan that was cut short — what a store swap
// during a graph hot-reload produces. The embedded nil Store is deliberate:
// only the sequencer may be reached on this path.
type abortingCensusStore struct{ graph.Store }

func (abortingCensusStore) SynthesizedEdgesSeq() (iter.Seq[graph.SynthesizedEdge], func() error) {
	return func(yield func(graph.SynthesizedEdge) bool) {
			yield(graph.SynthesizedEdge{
				From: "repo/a.go::A", To: "repo/b.go::B",
				Kind: graph.EdgeReferences, SynthesizedBy: "odoo",
			})
		}, func() error {
			return errors.New("database is closed")
		}
}

// Both response paths must refuse. The compact branch is the one that would
// otherwise print "no synthesized edges" — the exact false statement this
// change removes — so asserting only the JSON path would miss the regression
// that matters most.
func TestAnalyzeSynthesizersHandlerRefusesAnAbortedCensus(t *testing.T) {
	s := &Server{graph: abortingCensusStore{}}

	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{name: "json"},
		{name: "compact", args: map[string]any{"compact": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := mcplib.CallToolRequest{}
			req.Params.Name = "analyze"
			req.Params.Arguments = tc.args

			res, err := s.handleAnalyzeSynthesizers(context.Background(), req)
			require.NoError(t, err)
			require.NotNil(t, res)
			require.True(t, res.IsError, "an aborted census must be an error result, got: %s", resultText(res))

			text := resultText(res)
			require.NotContains(t, text, "no synthesized edges",
				"the handler must not report an aborted scan as an empty graph")
			require.Contains(t, text, "aborted")
		})
	}
}
