package mcp

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

func sourcePreservationCandidate(id string, textRank int, sourceSignal float64) *rerank.Candidate {
	candidate := &rerank.Candidate{
		Node:       &graph.Node{ID: id, Name: id, Kind: graph.KindFunction, FilePath: id + ".go"},
		TextRank:   textRank,
		VectorRank: -1,
	}
	if sourceSignal > 0 {
		candidate.Signals = map[string]float64{exploreSourceLiteralSignal: sourceSignal}
	}
	return candidate
}

func TestLimitExploreCandidatesPreservingSourceLiteralReservesOneFullCapSlot(t *testing.T) {
	candidates := []*rerank.Candidate{
		sourcePreservationCandidate("primary-0", 0, 0),
		sourcePreservationCandidate("primary-1", 1, 0),
		sourcePreservationCandidate("primary-2", 2, 0),
		sourcePreservationCandidate("primary-3", 3, 0),
		sourcePreservationCandidate("source", 80, 1),
	}

	require.Nil(t, candidateByID(limitExploreCandidates(candidates, 4), "source"), "the fixture must exercise a source candidate outside the ordinary cap")
	bounded := limitExploreCandidatesPreservingSourceLiteral(candidates, 4)
	require.Len(t, bounded, 4)
	require.NotNil(t, candidateByID(bounded, "source"))
}

func TestLimitExploreCandidatesPreservingSourceLiteralPrefersMultiAnchorOwner(t *testing.T) {
	multi := sourcePreservationCandidate("multi-anchor", 90, 0.25)
	multi.Signals[exploreSourceLiteralCoverageSignal] = 2
	single := sourcePreservationCandidate("single-anchor", 80, 1)
	single.Signals[exploreSourceLiteralCoverageSignal] = 1
	candidates := []*rerank.Candidate{
		sourcePreservationCandidate("primary-0", 0, 0),
		sourcePreservationCandidate("primary-1", 1, 0),
		single,
		multi,
	}

	bounded := limitExploreCandidatesPreservingSourceLiteral(candidates, 2)

	require.Len(t, bounded, 2)
	require.NotNil(t, candidateByID(bounded, multi.Node.ID))
	require.Nil(t, candidateByID(bounded, single.Node.ID), "a two-slot window preserves its semantic head plus the multi-anchor owner")
}

func TestSelectFinalExploreCandidatesReservesOnlyOneAmbiguousSingleAnchorOwner(t *testing.T) {
	first := sourcePreservationCandidate("ambiguous-a", 80, 1)
	first.Signals[exploreSourceLiteralCoverageSignal] = 1
	first.Signals[exploreContentRecallAmbiguousSignal] = 1
	second := sourcePreservationCandidate("ambiguous-b", 90, 0.5)
	second.Signals[exploreSourceLiteralCoverageSignal] = 1
	second.Signals[exploreContentRecallAmbiguousSignal] = 1
	prod := []*rerank.Candidate{
		sourcePreservationCandidate("primary-0", 0, 0),
		sourcePreservationCandidate("primary-1", 1, 0),
		sourcePreservationCandidate("primary-2", 2, 0),
		first,
		second,
	}

	selected := selectFinalExploreCandidates(prod, nil, 3)

	require.Len(t, selected, 3)
	require.Equal(t, "primary-0", selected[0].Node.ID)
	require.NotNil(t, candidateByID(selected, first.Node.ID))
	require.Nil(t, candidateByID(selected, second.Node.ID))
	require.NotNil(t, candidateByID(selected, "primary-1"), "ambiguous collision evidence must not consume both reserve slots")
}

func TestSelectFinalExploreCandidatesPrefersTaskAlignedAmbiguousCallee(t *testing.T) {
	generic := sourcePreservationCandidate("generic-register", 10, 1)
	generic.Node.Name = "Register"
	generic.Signals[exploreSourceLiteralCoverageSignal] = 1
	generic.Signals[exploreContentRecallAmbiguousSignal] = 1
	generic.Signals[exploreSourceLiteralCalleeSignal] = 1
	specific := sourcePreservationCandidate("specific-register", 20, 0.5)
	specific.Node.Name = "RegisterDefaultFormatter"
	specific.Node.Language = "rust"
	specific.Node.FilePath = "src/registry.rs"
	specific.Signals[exploreSourceLiteralCoverageSignal] = 1
	specific.Signals[exploreContentRecallAmbiguousSignal] = 1
	specific.Signals[exploreSourceLiteralCalleeSignal] = 1
	specific.Signals[exploreSourceLiteralTaskAlignSignal] = 1
	third := sourcePreservationCandidate("third-register", 30, 0.25)
	third.Signals[exploreSourceLiteralCoverageSignal] = 1
	third.Signals[exploreContentRecallAmbiguousSignal] = 1
	third.Signals[exploreSourceLiteralCalleeSignal] = 1
	prod := []*rerank.Candidate{
		sourcePreservationCandidate("primary-0", 0, 0),
		sourcePreservationCandidate("primary-1", 1, 0),
		sourcePreservationCandidate("primary-2", 2, 0),
		generic,
		specific,
		third,
	}

	selected := selectFinalExploreCandidates(prod, nil, 3)

	require.Len(t, selected, 3)
	require.Equal(t, "primary-0", selected[0].Node.ID, "source reservation must preserve the semantic head")
	require.NotNil(t, candidateByID(selected, specific.Node.ID))
	require.Nil(t, candidateByID(selected, generic.Node.ID), "one compact collision may reserve only one owner")
	require.Nil(t, candidateByID(selected, third.Node.ID), "three-way collision noise must stay outside the bounded answer")
}

func TestSelectFinalExploreCandidatesPrefersGraphResolvedCalleeWithinOneLiteralSeat(t *testing.T) {
	owner := sourcePreservationCandidate("settled-owner", 4, 1)
	owner.Signals[exploreSourceLiteralCoverageSignal] = 1
	callee := sourcePreservationCandidate("ambiguous-callee", 9, 0.5)
	callee.Signals[exploreSourceLiteralCoverageSignal] = 1
	callee.Signals[exploreContentRecallAmbiguousSignal] = 1
	callee.Signals[exploreSourceLiteralCalleeSignal] = 1
	selected := selectFinalExploreCandidates([]*rerank.Candidate{
		sourcePreservationCandidate("semantic-head", 0, 0), owner, callee,
	}, nil, 2)

	require.Len(t, selected, 2)
	require.Equal(t, "semantic-head", selected[0].Node.ID)
	require.Equal(t, callee.Node.ID, selected[1].Node.ID)
	require.Nil(t, candidateByID(selected, owner.Node.ID))
}

func TestSelectFinalExploreCandidatesDeduplicatesSourceOwnersAndHonorsSmallLimits(t *testing.T) {
	head := sourcePreservationCandidate("head", 0, 0)
	source := sourcePreservationCandidate("aligned-source", 8, 0.5)
	source.Signals[exploreSourceLiteralCoverageSignal] = 1
	source.Signals[exploreContentRecallAmbiguousSignal] = 1
	source.Signals[exploreSourceLiteralCalleeSignal] = 1
	source.Signals[exploreSourceLiteralTaskAlignSignal] = 1
	duplicate := *source
	duplicate.Signals = map[string]float64{
		exploreSourceLiteralSignal:          0.5,
		exploreSourceLiteralCoverageSignal:  1,
		exploreContentRecallAmbiguousSignal: 1,
		exploreSourceLiteralCalleeSignal:    1,
		exploreSourceLiteralTaskAlignSignal: 1,
	}
	prod := []*rerank.Candidate{head, sourcePreservationCandidate("tail", 1, 0), source, &duplicate}

	require.Nil(t, selectFinalExploreCandidates(prod, nil, 0))
	one := selectFinalExploreCandidates(prod, nil, 1)
	require.Len(t, one, 1)
	require.Equal(t, "head", one[0].Node.ID)
	two := selectFinalExploreCandidates(prod, nil, 2)
	require.Len(t, two, 2)
	require.Equal(t, "head", two[0].Node.ID)
	require.Equal(t, "aligned-source", two[1].Node.ID)
}

func TestSelectFinalExploreCandidatesReservesTwoSourceOwnersWhenCapacityAllows(t *testing.T) {
	multi := sourcePreservationCandidate("multi-anchor", 90, 0.25)
	multi.Signals[exploreSourceLiteralCoverageSignal] = 2
	single := sourcePreservationCandidate("single-anchor", 80, 1)
	single.Signals[exploreSourceLiteralCoverageSignal] = 1
	prod := []*rerank.Candidate{
		sourcePreservationCandidate("primary-0", 0, 0),
		sourcePreservationCandidate("primary-1", 1, 0),
		sourcePreservationCandidate("primary-2", 2, 0),
		single,
		multi,
	}

	selected := selectFinalExploreCandidates(prod, nil, 3)

	require.Len(t, selected, 3)
	require.Equal(t, multi.Node.ID, selected[0].Node.ID)
	require.NotNil(t, candidateByID(selected, single.Node.ID))
	require.NotNil(t, candidateByID(selected, "primary-0"), "bounded reservation must retain the semantic head")
}

func TestSelectFinalExploreCandidatesPreservesSourceLiteralAfterRerank(t *testing.T) {
	prod := []*rerank.Candidate{
		sourcePreservationCandidate("primary-0", 0, 0),
		sourcePreservationCandidate("primary-1", 1, 0),
		sourcePreservationCandidate("primary-2", 2, 0),
		sourcePreservationCandidate("source", 80, 1),
	}
	tests := []*rerank.Candidate{sourcePreservationCandidate("test", 0, 0)}

	require.Nil(t, candidateByID(prod[:3], "source"), "the fixture must put source evidence outside the final reranked window")
	selected := selectFinalExploreCandidates(prod, tests, 3)

	require.Len(t, selected, 3)
	require.Equal(t, []string{"source", "primary-0", "primary-1"}, []string{
		selected[0].Node.ID,
		selected[1].Node.ID,
		selected[2].Node.ID,
	})
	require.Nil(t, candidateByID(selected, "test"), "source reservation must replace, not widen, the production cap")
}

func TestSelectFinalExploreCandidatesPromotesCompleteSourceForReversedInput(t *testing.T) {
	for name, prod := range map[string][]*rerank.Candidate{
		"source-last": {
			sourcePreservationCandidate("primary", 0, 0),
			sourcePreservationCandidate("source", 1, 1),
		},
		"source-first": {
			sourcePreservationCandidate("source", 1, 1),
			sourcePreservationCandidate("primary", 0, 0),
		},
	} {
		t.Run(name, func(t *testing.T) {
			selected := selectFinalExploreCandidates(prod, nil, 2)
			require.Len(t, selected, 2)
			require.Equal(t, "source", selected[0].Node.ID)
		})
	}
}

func TestSelectFinalExploreCandidatesKeepsAmbiguousSourceBehindOrdinaryHead(t *testing.T) {
	primary := sourcePreservationCandidate("primary", 0, 0)
	ambiguous := sourcePreservationCandidate("source", 1, 1)
	ambiguous.Signals[exploreContentRecallAmbiguousSignal] = 1

	selected := selectFinalExploreCandidates([]*rerank.Candidate{primary, ambiguous}, nil, 2)

	require.Len(t, selected, 2)
	require.Equal(t, "primary", selected[0].Node.ID)
	require.Equal(t, "source", selected[1].Node.ID)
}

func TestSelectFinalExploreCandidatesKeepsSettledProductionOrder(t *testing.T) {
	prod := []*rerank.Candidate{
		sourcePreservationCandidate("settled-0", 80, 0),
		sourcePreservationCandidate("settled-1", 70, 0),
		sourcePreservationCandidate("earlier-channel-rank", 0, 0),
	}

	selected := selectFinalExploreCandidates(prod, nil, 2)

	require.Len(t, selected, 2)
	require.Equal(t, "settled-0", selected[0].Node.ID)
	require.Equal(t, "settled-1", selected[1].Node.ID)
}

func TestMergeExploreCandidatesPreservesSourceLiteralSignalThroughDedupe(t *testing.T) {
	node := &graph.Node{ID: "same", Name: "same", Kind: graph.KindFunction, FilePath: "same.go"}
	primarySignals := map[string]float64{"ordinary": 1}
	merged := mergeExploreCandidates(
		[]*rerank.Candidate{{Node: node, TextRank: 0, VectorRank: -1, Signals: primarySignals}},
		[]*rerank.Candidate{{Node: node, TextRank: 9, VectorRank: -1, Signals: map[string]float64{
			exploreSourceLiteralSignal: 0.5, exploreSourceLiteralCoverageSignal: 2,
		}}},
		20,
	)

	require.Len(t, merged, 1)
	require.Equal(t, 0.5, merged[0].Signals[exploreSourceLiteralSignal])
	require.Equal(t, float64(2), merged[0].Signals[exploreSourceLiteralCoverageSignal])
	require.Zero(t, primarySignals[exploreSourceLiteralSignal], "request-local evidence must not mutate an input candidate")
	require.Zero(t, primarySignals[exploreSourceLiteralCoverageSignal], "coverage evidence must remain request-local")
}

func TestReserveExploreConceptImplementationHandlesMoreThanInlineTermCapacity(t *testing.T) {
	terms := make([]string, 65)
	for i := range terms {
		terms[i] = fmt.Sprintf("term%c%c", 'a'+rune(i/26), 'a'+rune(i%26))
	}
	primary := sourcePreservationCandidate("primary", 0, 0)
	targetName := terms[0] + "_" + terms[1]
	target := &rerank.Candidate{Node: &graph.Node{
		ID:       "demo/worker.go::" + targetName,
		Name:     targetName,
		QualName: "demo." + targetName,
		Kind:     graph.KindFunction,
		FilePath: "demo/worker.go",
	}}
	candidates := []*rerank.Candidate{
		primary,
		sourcePreservationCandidate("secondary", 1, 0),
		target,
	}

	got, protected := reserveExploreConceptImplementation(
		strings.Join(terms, " "),
		rerank.QueryClassConcept,
		candidates,
		2,
	)

	require.Len(t, got, len(candidates))
	require.Same(t, primary, got[0], "reservation must preserve the semantic head")
	require.Same(t, target, got[1], "the callable matching long-query terms must be reserved")
	require.Equal(t, target.Node.ID, protected)
}

func TestExploreAnswerReadyKeepsQuotedNonExactConceptNonTerminal(t *testing.T) {
	head := exploreTarget{
		node: &graph.Node{
			ID:       "demo/registry.go::locale_registry_pipeline",
			Name:     "locale_registry_pipeline",
			QualName: "demo.locale_registry_pipeline",
			Kind:     graph.KindFunction,
			FilePath: "demo/registry.go",
		},
		source:                "func locale_registry_pipeline() {}",
		conceptImplementation: true,
	}

	require.True(t, exploreAnswerReady("locate locale registry pipeline", []exploreTarget{head}), "the fixture must otherwise satisfy ordinary concept terminality")
	require.False(t, exploreAnswerReady(`locate locale registry pipeline for "ku"`, []exploreTarget{head}))
}

func TestLocalizationEvidenceOrdersEligibleLiteralRowsByDistinctMatches(t *testing.T) {
	semantic := exploreTarget{node: &graph.Node{ID: "repo/semantic.go::Semantic", Name: "Semantic", FilePath: "repo/semantic.go"}}
	low := exploreTarget{
		node:          &graph.Node{ID: "repo/low.go::Low", Name: "Low", FilePath: "repo/low.go"},
		sourceLiteral: true, literalPrimaryEligible: true, literalMatchCount: 1,
	}
	high := exploreTarget{
		node:          &graph.Node{ID: "repo/high.go::High", Name: "High", FilePath: "repo/high.go"},
		sourceLiteral: true, literalPrimaryEligible: true, literalMatchCount: 3,
	}

	ordered := localizationEvidenceTargetsFromDraft("find the handler", "", []exploreTarget{semantic, low, high}, nil)
	require.Len(t, ordered, 3)
	require.Equal(t, semantic.node.ID, ordered[0].node.ID)
	require.Equal(t, high.node.ID, ordered[1].node.ID)
	require.Equal(t, low.node.ID, ordered[2].node.ID)
}

func BenchmarkLimitExploreCandidatesPreservingSourceLiteral80(b *testing.B) {
	candidates := make([]*rerank.Candidate, 0, 80)
	for i := 0; i < 79; i++ {
		candidates = append(candidates, sourcePreservationCandidate(fmt.Sprintf("primary-%02d", i), i, 0))
	}
	candidates = append(candidates, sourcePreservationCandidate("source", 200, 1))

	b.ReportAllocs()
	for b.Loop() {
		if got := limitExploreCandidatesPreservingSourceLiteral(candidates, 40); len(got) != 40 {
			b.Fatalf("candidate count = %d, want 40", len(got))
		}
	}
}
