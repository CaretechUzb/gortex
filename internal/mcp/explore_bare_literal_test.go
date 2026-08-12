package mcp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/search/rerank"
)

func TestExploreBareLiteralRecallTermsKeepsDiagnosticTail(t *testing.T) {
	terms := exploreBareLiteralRecallTerms("The daemon reports unable to seal chunk manifest after the retry starts.")
	require.Equal(t, []string{"unable to seal chunk manifest"}, terms)
}

func TestExploreBareLiteralRecallTermsAdmitsAllCapsAndCuedStructuredValues(t *testing.T) {
	terms := exploreBareLiteralRecallTerms("Run ZRANGEBYSCORE again with the header X-CACHE-MODE value.")
	require.Equal(t, []string{"X-CACHE-MODE", "ZRANGEBYSCORE"}, terms)
}

func TestExploreBareLiteralRecallTermsExcludesExplicitAndFencedLiterals(t *testing.T) {
	task := "The daemon logs \"UNABLE TO SEAL CHUNK MANIFEST\" for `CACHE_MISS`.\n\n" +
		"```text\nERROR: DURABLE SNAPSHOT MISSING\n```\n" +
		"    TRACE BUFFER OVERFLOW\n"
	require.Empty(t, exploreBareLiteralRecallTerms(task))
}

func TestExploreBareLiteralRecallTermsRejectsNoise(t *testing.T) {
	task := "The service reports https://EXAMPLE.COM before retrying version V1-2-3 at ABCDEF1234. VERSION remains unchanged."
	require.Empty(t, exploreBareLiteralRecallTerms(task))
}

func TestExploreBareLiteralRecallTermsDoesNotDuplicateSyntacticLane(t *testing.T) {
	terms := exploreBareLiteralRecallTerms("Retry CACHE_KEY through ZRANGEBYSCORE when the command stalls.")
	require.Equal(t, []string{"ZRANGEBYSCORE"}, terms)
}

func TestExploreBareLiteralRecallTermsDeduplicatesAndCaps(t *testing.T) {
	terms := exploreBareLiteralRecallTerms("ALPHA alpha BRAVO CHARLIE DELTA ECHO FOXTROT GOLF ALPHA")
	require.Equal(t, []string{"ALPHA", "BRAVO", "CHARLIE", "DELTA", "ECHO"}, terms)
}

func TestExploreBareLiteralRecallTermsBoundsTaskScan(t *testing.T) {
	task := strings.Repeat("ordinary prose ", exploreBareLiteralMaxTaskBytes/8) + " ZRANGEBYSCORE"
	require.Empty(t, exploreBareLiteralRecallTerms(task))
}

func TestExploreBareLiteralRecallTermsDoesNotCreateQuotedClaims(t *testing.T) {
	task := "The daemon reports unable to seal chunk manifest before retrying ZRANGEBYSCORE."
	require.NotEmpty(t, exploreBareLiteralRecallTerms(task))
	require.Empty(t, exploreQuotedRecallTerms(task))
	require.Empty(t, exploreQuotedRecallClaimTerms(task))
}

func TestExploreBareLiteralLaneEligibleOnlyForUnanchoredConcepts(t *testing.T) {
	unanchoredTask := "Recovery loses the registry entry during rollback"
	require.True(t, exploreBareLiteralLaneEligible(
		unanchoredTask, unanchoredTask, true, false, nil, nil,
	))

	explicit := []*rerank.Candidate{{Node: &graph.Node{
		ID: "demo/registry.go::FlushRegistry", Name: "FlushRegistry",
		Kind: graph.KindFunction, FilePath: "demo/registry.go",
	}}}
	cases := []struct {
		name          string
		task          string
		query         string
		concept       bool
		artifactReady bool
		candidates    []*rerank.Candidate
		protected     map[int]string
	}{
		{name: "quoted", task: `recovery logs "registry entry missing"`, query: unanchoredTask, concept: true},
		{name: "path", task: unanchoredTask, query: "fix src/cache.go during recovery", concept: true},
		{name: "ranked explicit call", task: unanchoredTask, query: "FlushRegistry() loses entries", concept: true, candidates: explicit},
		{name: "protected syntax", task: unanchoredTask, query: unanchoredTask, concept: true, protected: map[int]string{0: "node"}},
		{name: "artifact ready", task: unanchoredTask, query: unanchoredTask, concept: true, artifactReady: true},
		{name: "non concept", task: unanchoredTask, query: unanchoredTask},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			require.False(t, exploreBareLiteralLaneEligible(
				test.task, test.query, test.concept, test.artifactReady,
				test.candidates, test.protected,
			))
		})
	}
}
