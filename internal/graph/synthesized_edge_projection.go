package graph

// PURPOSE — the streaming projection behind `analyze kind=synthesizers`: every
// edge carrying a synthesized_by stamp, paired with a terminal-error check so
// an aborted scan cannot be mistaken for an empty graph.
// RATIONALE — the analyzer used to materialise Store.AllEdges(), which on a
// multi-million-edge store both missed the tool deadline and returned nil when
// the read was cut short, reporting "no synthesizers fired" as a normal answer.
// KEYWORDS — synthesizers, census, projection, silent-failure

import "iter"

// SynthesizedEdge is the projection the synthesizer census aggregates: the
// logical edge identity plus the three metadata fields the rollup reports.
//
// It deliberately does NOT reuse FrameworkCensusEdge. That projection's
// per-kind field policy withholds SynthesizedBy from every kind except
// references and imports — correct for framework admission, which must not
// treat a synthesized call as a candidate, but it would silently drop the
// extends / reads / composes / overrides / calls / renders_child edges that
// also carry the stamp. Sharing the type would make a census of "every
// synthesized edge" quietly mean "some of them".
type SynthesizedEdge struct {
	From          string
	To            string
	Kind          EdgeKind
	SynthesizedBy string
	Provenance    string
	Via           string
}

// SynthesizedEdgeSequencer streams every edge carrying a synthesized_by stamp.
//
// The second return is a terminal-error check in the shape of sql.Rows.Err:
// ranging to exhaustion is NOT proof the scan completed. A backend whose read
// is aborted mid-cursor — a closed store or a store swap during a graph
// hot-reload — ends the sequence exactly like a clean exhaust, and a caller
// that skipped the check would publish the truncated tally as a whole-graph
// answer. Check it before reporting a total.
type SynthesizedEdgeSequencer interface {
	SynthesizedEdgesSeq() (iter.Seq[SynthesizedEdge], func() error)
}

// SynthesizedEdgesSeq selects the streaming projection when the backend
// provides one and falls back to materialising every edge otherwise.
//
// The fallback exists for in-memory graphs and test doubles. It cannot detect
// an aborted read — the Store iterator it walks has no way to report one — so
// it always reports a nil error. That is honest for a *Graph, whose read cannot
// fail part-way, and it is the reason the assertion below is made against s
// directly.
//
// Deliberately NOT resolved through a shadow's owner, unlike the enrichment
// bookkeeping in internal/semantic. That rule applies to state with no
// in-memory representation; edges have one. An overlay session pushes unsaved
// buffers as a shadow graph holding edges the durable store does not, so
// walking to the owner here would answer an overlay's census from the wrong
// graph — silently, and in the direction of missing data.
// Takes a Reader, not a Store: the census only ever walks edges, and the MCP
// tool that drives it resolves an overlay session to a Reader. Every Store is
// a Reader, so this widened in the 2026-09-04 merge without touching a caller,
// and the streaming fast path below is still selected by type assertion.
func SynthesizedEdgesSeq(s Reader) (iter.Seq[SynthesizedEdge], func() error) {
	if s == nil {
		return func(func(SynthesizedEdge) bool) {}, func() error { return nil }
	}
	if seq, ok := s.(SynthesizedEdgeSequencer); ok {
		return seq.SynthesizedEdgesSeq()
	}
	return func(yield func(SynthesizedEdge) bool) {
		for _, edge := range s.AllEdges() {
			if edge == nil || edge.Meta == nil {
				continue
			}
			by, _ := edge.Meta["synthesized_by"].(string)
			if by == "" {
				continue
			}
			row := SynthesizedEdge{From: edge.From, To: edge.To, Kind: edge.Kind, SynthesizedBy: by}
			row.Provenance, _ = edge.Meta["provenance"].(string)
			row.Via, _ = edge.Meta["via"].(string)
			if !yield(row) {
				return
			}
		}
	}, func() error { return nil }
}
