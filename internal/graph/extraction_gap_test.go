package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildClassifyGraph wires three functions in one file, each with a
// distinct edge profile, so ClassifyZeroEdge can be exercised against
// every classification:
//
//   - Used      — the file defines it AND a caller calls it.
//   - Unused    — the file defines it, it makes an outgoing call, but
//     nothing calls it back. Indexed, dead.
//   - Orphaned  — added with no edges at all, mimicking a symbol the
//     extractor never processed.
func buildClassifyGraph() *Graph {
	g := New()
	g.AddNode(&Node{ID: "svc.go", Kind: KindFile, Name: "svc.go", FilePath: "svc.go"})
	g.AddNode(&Node{ID: "svc.go::Caller", Kind: KindFunction, Name: "Caller", FilePath: "svc.go"})
	g.AddNode(&Node{ID: "svc.go::Used", Kind: KindFunction, Name: "Used", FilePath: "svc.go"})
	g.AddNode(&Node{ID: "svc.go::Unused", Kind: KindFunction, Name: "Unused", FilePath: "svc.go"})
	g.AddNode(&Node{ID: "svc.go::Helper", Kind: KindFunction, Name: "Helper", FilePath: "svc.go"})

	// The file defines every function except the orphan.
	g.AddEdge(&Edge{From: "svc.go", To: "svc.go::Caller", Kind: EdgeDefines})
	g.AddEdge(&Edge{From: "svc.go", To: "svc.go::Used", Kind: EdgeDefines})
	g.AddEdge(&Edge{From: "svc.go", To: "svc.go::Unused", Kind: EdgeDefines})
	g.AddEdge(&Edge{From: "svc.go", To: "svc.go::Helper", Kind: EdgeDefines})

	// Used has an incoming call edge the resolver bound unambiguously.
	// The provenance is load-bearing: classification weighs incoming
	// usage edges by tier, and an unstamped zero-confidence call edge
	// backfills to text_matched — the weak tier — so leaving it off
	// would make this fixture assert coverage_incomplete, not none.
	g.AddEdge(&Edge{From: "svc.go::Caller", To: "svc.go::Used", Kind: EdgeCalls,
		Origin: OriginASTResolved, Confidence: 0.9})
	// Unused has only an outgoing call — no incoming usage edge.
	g.AddEdge(&Edge{From: "svc.go::Unused", To: "svc.go::Helper", Kind: EdgeCalls,
		Origin: OriginASTResolved, Confidence: 0.9})
	return g
}

func TestClassifyZeroEdge(t *testing.T) {
	g := buildClassifyGraph()
	// A method node with only its member_of structural edge — indexed,
	// but nothing references it.
	g.AddNode(&Node{ID: "svc.go::T", Kind: KindType, Name: "T", FilePath: "svc.go"})
	g.AddNode(&Node{ID: "svc.go::T.Method", Kind: KindMethod, Name: "Method", FilePath: "svc.go"})
	g.AddEdge(&Edge{From: "svc.go::T.Method", To: "svc.go::T", Kind: EdgeMemberOf})
	// An orphan: in the graph, but carrying no edges of any kind.
	g.AddNode(&Node{ID: "svc.go::Orphan", Kind: KindFunction, Name: "Orphan", FilePath: "svc.go"})

	tests := []struct {
		name     string
		symbolID string
		want     ZeroEdgeClass
	}{
		{
			name:     "incoming call edge yields none",
			symbolID: "svc.go::Used",
			want:     ZeroEdgeNone,
		},
		{
			name:     "only structural defines plus outgoing call yields likely_unused",
			symbolID: "svc.go::Unused",
			want:     ZeroEdgeLikelyUnused,
		},
		{
			name:     "method with only member_of yields likely_unused",
			symbolID: "svc.go::T.Method",
			want:     ZeroEdgeLikelyUnused,
		},
		{
			name:     "zero edges of any kind yields possible_extraction_gap",
			symbolID: "svc.go::Orphan",
			want:     ZeroEdgePossibleExtractionGap,
		},
		{
			name:     "unknown symbol id yields possible_extraction_gap",
			symbolID: "svc.go::DoesNotExist",
			want:     ZeroEdgePossibleExtractionGap,
		},
		{
			name:     "empty symbol id yields possible_extraction_gap",
			symbolID: "",
			want:     ZeroEdgePossibleExtractionGap,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ClassifyZeroEdge(g, tt.symbolID)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClassifyZeroEdge_NilGraph(t *testing.T) {
	assert.Equal(t, ZeroEdgePossibleExtractionGap, ClassifyZeroEdge(nil, "svc.go::Used"))
}

func TestCaveatForZeroEdge(t *testing.T) {
	g := buildClassifyGraph()
	g.AddNode(&Node{ID: "svc.go::Orphan", Kind: KindFunction, Name: "Orphan", FilePath: "svc.go"})

	// A used symbol carries no caveat — nil is returned so callers can
	// attach it unconditionally.
	assert.Nil(t, CaveatForZeroEdge(g, "svc.go::Used"))

	// Likely-unused: classification plus a non-empty message.
	unused := CaveatForZeroEdge(g, "svc.go::Unused")
	require.NotNil(t, unused)
	assert.Equal(t, ZeroEdgeLikelyUnused, unused.Class)
	assert.NotEmpty(t, unused.Message)

	// Extraction gap: classification plus a non-empty message.
	gap := CaveatForZeroEdge(g, "svc.go::Orphan")
	require.NotNil(t, gap)
	assert.Equal(t, ZeroEdgePossibleExtractionGap, gap.Class)
	assert.NotEmpty(t, gap.Message)

	// The two caveat messages must differ — they describe opposite
	// situations and an agent has to be able to tell them apart.
	assert.NotEqual(t, unused.Message, gap.Message)
}
