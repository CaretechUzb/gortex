package graph

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildProvenanceGraph gives one type three methods that differ only in
// the provenance of their single incoming call edge, so classification can
// be exercised against the tier ladder with everything else held equal.
func buildProvenanceGraph() *Graph {
	g := New()
	g.AddNode(&Node{ID: "svc.go", Kind: KindFile, Name: "svc.go", FilePath: "svc.go", Language: "go"})
	g.AddNode(&Node{ID: "svc.go::T", Kind: KindType, Name: "T", FilePath: "svc.go", Language: "go"})
	for _, name := range []string{"Caller", "Strong", "Weak", "Untagged", "Speculated", "Inferred"} {
		id := "svc.go::T." + name
		g.AddNode(&Node{ID: id, Kind: KindMethod, Name: name, FilePath: "svc.go", Language: "go"})
		g.AddEdge(&Edge{From: "svc.go", To: id, Kind: EdgeDefines})
	}
	call := func(to string, e *Edge) {
		e.From = "svc.go::T.Caller"
		e.To = to
		e.Kind = EdgeCalls
		g.AddEdge(e)
	}
	// Unambiguous AST binding — real evidence of use.
	call("svc.go::T.Strong", &Edge{Origin: OriginASTResolved, Confidence: 0.9})
	// Bare-name match — the tier a common method name collects false
	// positives in.
	call("svc.go::T.Weak", &Edge{Origin: OriginTextMatched})
	// Producer never stamped an Origin. EffectiveOrigin backfills it from
	// kind + confidence, which for a zero-confidence call edge is
	// text_matched — the same tier the wire response displays.
	call("svc.go::T.Untagged", &Edge{})
	// Best-guess dynamic dispatch: default-excluded from every
	// edge-returning surface, so it cannot serve as proof of use.
	call("svc.go::T.Speculated", &Edge{Meta: map[string]any{MetaSpeculative: true}})
	// Heuristic type inference is keyed on an extracted type, not on a
	// name collision — real evidence.
	call("svc.go::T.Inferred", &Edge{Origin: OriginASTInferred, Confidence: 0.7})
	return g
}

// A symbol whose only incoming usage edges are name-only matches is not
// proven used. Reporting it as ZeroEdgeNone suppresses the caveat entirely,
// so the noise reads as proof the symbol is live and genuine dead code
// hides behind it — issue #433.
func TestClassifyZeroEdge_WeighsUsageEdgeProvenance(t *testing.T) {
	g := buildProvenanceGraph()

	tests := []struct {
		name     string
		symbolID string
		want     ZeroEdgeClass
	}{
		{
			name:     "ast_resolved call proves use",
			symbolID: "svc.go::T.Strong",
			want:     ZeroEdgeNone,
		},
		{
			name:     "ast_inferred call proves use",
			symbolID: "svc.go::T.Inferred",
			want:     ZeroEdgeNone,
		},
		{
			name:     "text_matched call alone is unverified coverage",
			symbolID: "svc.go::T.Weak",
			want:     ZeroEdgeCoverageIncomplete,
		},
		{
			name:     "unstamped zero-confidence call alone is unverified coverage",
			symbolID: "svc.go::T.Untagged",
			want:     ZeroEdgeCoverageIncomplete,
		},
		{
			name:     "speculative call alone is unverified coverage",
			symbolID: "svc.go::T.Speculated",
			want:     ZeroEdgeCoverageIncomplete,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ClassifyZeroEdge(g, tt.symbolID))
		})
	}
}

// One trustworthy caller is enough: weak edges alongside it must not
// downgrade the verdict, or every common-named symbol with real callers
// would collect a spurious caveat.
func TestClassifyZeroEdge_StrongEdgeOutweighsWeakOnes(t *testing.T) {
	g := buildProvenanceGraph()
	g.AddEdge(&Edge{From: "svc.go::T.Caller", To: "svc.go::T.Weak", Kind: EdgeCalls,
		FilePath: "other.go", Line: 12, Origin: OriginLSPResolved, Confidence: 1})

	assert.Equal(t, ZeroEdgeNone, ClassifyZeroEdge(g, "svc.go::T.Weak"),
		"a symbol with one compiler-grade caller is used, however much name-match noise rides along")
}

// The weak-evidence verdict must not swallow the likely_unused signal: a
// symbol with no incoming usage edge at all is still the dead-code case.
func TestClassifyZeroEdge_NoUsageEdgeStaysLikelyUnused(t *testing.T) {
	g := buildProvenanceGraph()
	g.AddNode(&Node{ID: "svc.go::T.Dead", Kind: KindMethod, Name: "Dead", FilePath: "svc.go", Language: "go"})
	g.AddEdge(&Edge{From: "svc.go", To: "svc.go::T.Dead", Kind: EdgeDefines})

	assert.Equal(t, ZeroEdgeLikelyUnused, ClassifyZeroEdge(g, "svc.go::T.Dead"))
}

// A structural incoming edge is not a usage edge and must not be mistaken
// for weak usage evidence — `defines` alone stays likely_unused.
func TestClassifyZeroEdge_StructuralEdgeIsNotWeakUsage(t *testing.T) {
	g := New()
	g.AddNode(&Node{ID: "svc.go", Kind: KindFile, Name: "svc.go", FilePath: "svc.go"})
	g.AddNode(&Node{ID: "svc.go::Lonely", Kind: KindFunction, Name: "Lonely", FilePath: "svc.go"})
	// An unstamped zero-confidence `defines` edge: DefaultOriginFor treats
	// structural kinds as ast_resolved regardless of confidence, but it is
	// not a usage kind either way.
	g.AddEdge(&Edge{From: "svc.go", To: "svc.go::Lonely", Kind: EdgeDefines})

	assert.Equal(t, ZeroEdgeLikelyUnused, ClassifyZeroEdge(g, "svc.go::Lonely"))
}

func TestCaveatForZeroEdge_WeakEvidenceCarriesCoverageIncomplete(t *testing.T) {
	g := buildProvenanceGraph()

	weak := CaveatForZeroEdge(g, "svc.go::T.Weak")
	require.NotNil(t, weak, "a weak-evidence symbol must carry a caveat, not nil")
	assert.Equal(t, ZeroEdgeCoverageIncomplete, weak.Class)
	assert.NotEmpty(t, weak.Message)

	assert.Nil(t, CaveatForZeroEdge(g, "svc.go::T.Strong"),
		"a symbol with trustworthy callers carries no caveat")
}

func TestWeakUsageEvidenceOnly(t *testing.T) {
	strong := &Edge{Kind: EdgeCalls, Origin: OriginASTResolved, Confidence: 0.9}
	weak := &Edge{Kind: EdgeCalls, Origin: OriginTextMatched}
	untagged := &Edge{Kind: EdgeCalls}
	structural := &Edge{Kind: EdgeDefines}

	tests := []struct {
		name  string
		edges []*Edge
		want  bool
	}{
		{name: "no edges at all", edges: nil, want: false},
		{name: "only structural edges", edges: []*Edge{structural}, want: false},
		{name: "single weak usage edge", edges: []*Edge{weak}, want: true},
		{name: "untagged zero-confidence call is weak", edges: []*Edge{untagged}, want: true},
		{name: "weak usage edges plus a structural edge", edges: []*Edge{weak, structural}, want: true},
		{name: "one strong edge among weak ones", edges: []*Edge{weak, strong, weak}, want: false},
		{name: "only strong edges", edges: []*Edge{strong}, want: false},
		{name: "nil entries are skipped", edges: []*Edge{nil, weak}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, WeakUsageEvidenceOnly(tt.edges))
		})
	}
}

// The populated-result caveat shares the machine-checkable class with the
// empty-result one — so a gate branching on coverage_incomplete trips for
// both — but must not reuse its "this empty result" wording.
func TestCaveatForWeakUsageEvidence(t *testing.T) {
	c := CaveatForWeakUsageEvidence()
	require.NotNil(t, c)
	assert.Equal(t, ZeroEdgeCoverageIncomplete, c.Class)
	assert.NotEmpty(t, c.Message)
	assert.NotEqual(t, zeroEdgeMessages[ZeroEdgeCoverageIncomplete], c.Message,
		"a populated result needs wording that does not call itself empty")
}

func TestIsWeakUsageEvidence(t *testing.T) {
	tests := []struct {
		name string
		edge *Edge
		want bool
	}{
		{name: "nil edge is weak", edge: nil, want: true},
		{name: "lsp_resolved", edge: &Edge{Kind: EdgeCalls, Origin: OriginLSPResolved, Confidence: 1}, want: false},
		{name: "lsp_dispatch", edge: &Edge{Kind: EdgeImplements, Origin: OriginLSPDispatch, Confidence: 1}, want: false},
		{name: "ast_resolved", edge: &Edge{Kind: EdgeCalls, Origin: OriginASTResolved, Confidence: 0.9}, want: false},
		{name: "ast_inferred", edge: &Edge{Kind: EdgeCalls, Origin: OriginASTInferred, Confidence: 0.7}, want: false},
		{name: "text_matched", edge: &Edge{Kind: EdgeCalls, Origin: OriginTextMatched}, want: true},
		{name: "speculative origin", edge: &Edge{Kind: EdgeCalls, Origin: OriginSpeculative, Confidence: 0.4}, want: true},
		{
			name: "speculative meta flag outweighs a strong origin",
			edge: &Edge{Kind: EdgeCalls, Origin: OriginASTResolved, Confidence: 0.9,
				Meta: map[string]any{MetaSpeculative: true}},
			want: true,
		},
		{
			name: "unstamped tests edge rides its structural-kind default",
			edge: &Edge{Kind: EdgeImplements},
			want: false,
		},
		{name: "unstamped zero-confidence call", edge: &Edge{Kind: EdgeCalls}, want: true},
		{
			name: "unstamped high-confidence call backfills to ast_resolved",
			edge: &Edge{Kind: EdgeCalls, Confidence: 0.95},
			want: false,
		},
		{
			name: "unstamped call with semantic_source backfills to lsp",
			edge: &Edge{Kind: EdgeCalls, Meta: map[string]any{"semantic_source": "gopls"}},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsWeakUsageEvidence(tt.edge))
		})
	}
}

func TestEffectiveOrigin(t *testing.T) {
	tests := []struct {
		name string
		edge *Edge
		want string
	}{
		{name: "nil edge reports the trusted baseline", edge: nil, want: OriginASTResolved},
		{
			name: "a stamped Origin wins over the confidence backfill",
			edge: &Edge{Kind: EdgeCalls, Origin: OriginTextMatched, Confidence: 1},
			want: OriginTextMatched,
		},
		{
			name: "unstamped call falls to its confidence tier",
			edge: &Edge{Kind: EdgeCalls, Confidence: 0.7},
			want: OriginASTInferred,
		},
		{
			name: "unstamped zero-confidence call is text_matched",
			edge: &Edge{Kind: EdgeCalls},
			want: OriginTextMatched,
		},
		{
			name: "unstamped structural edge is ast_resolved regardless of confidence",
			edge: &Edge{Kind: EdgeDefines},
			want: OriginASTResolved,
		},
		{
			name: "semantic_source promotes an unstamped edge to lsp",
			edge: &Edge{Kind: EdgeCalls, Meta: map[string]any{"semantic_source": "gopls"}},
			want: OriginLSPResolved,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.edge.EffectiveOrigin())
		})
	}
}
