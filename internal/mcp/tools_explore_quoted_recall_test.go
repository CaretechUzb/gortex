package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

type quotedRecallCountingStore struct {
	*store_sqlite.Store
	hits         map[string][]graph.ContentHit
	searchLimits []int
	searchRows   []int
	graphLookups int
}

func (s *quotedRecallCountingStore) SearchContent(term, _ string, limit int) ([]graph.ContentHit, error) {
	hits := s.hits[term]
	if limit < len(hits) {
		hits = hits[:limit]
	}
	s.searchLimits = append(s.searchLimits, limit)
	s.searchRows = append(s.searchRows, len(hits))
	return append([]graph.ContentHit(nil), hits...), nil
}

func (s *quotedRecallCountingStore) GetNodesByIDs(ids []string) map[string]*graph.Node {
	s.graphLookups++
	return s.Store.GetNodesByIDs(ids)
}

func newQuotedRecallCountingServer(t testing.TB, hits map[string][]graph.ContentHit) (*Server, *quotedRecallCountingStore) {
	t.Helper()
	store, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	seen := make(map[string]struct{})
	nodes := make([]*graph.Node, 0)
	for _, termHits := range hits {
		for _, hit := range termHits {
			if hit.NodeID == "" {
				continue
			}
			if _, exists := seen[hit.NodeID]; exists {
				continue
			}
			seen[hit.NodeID] = struct{}{}
			nodes = append(nodes, &graph.Node{
				ID: hit.NodeID, Name: "candidate", QualName: hit.NodeID,
				Kind: graph.KindFunction, FilePath: hit.FilePath, RepoPrefix: "demo", Language: "go",
			})
		}
	}
	store.AddBatch(nodes, nil)
	counting := &quotedRecallCountingStore{Store: store, hits: hits}
	return &Server{graph: counting}, counting
}

func quotedRecallHits(term string, count, exactAt int) []graph.ContentHit {
	hits := make([]graph.ContentHit, count)
	for i := range hits {
		id := fmt.Sprintf("demo/%s_%02d.go::candidate", term, i)
		snippet := term + "suffix"
		if i == exactAt {
			snippet = fmt.Sprintf(`case %q: register()`, term)
		}
		hits[i] = graph.ContentHit{NodeID: id, FilePath: fmt.Sprintf("demo/%s_%02d.go", term, i), Snippet: snippet}
	}
	return hits
}

func candidateByID(candidates []*rerank.Candidate, id string) *rerank.Candidate {
	for _, candidate := range candidates {
		if candidate != nil && candidate.Node != nil && candidate.Node.ID == id {
			return candidate
		}
	}
	return nil
}

func TestExploreQuotedRecallTermsAdmitsSixExplicitLiterals(t *testing.T) {
	terms := exploreQuotedRecallTerms(`find "alpha", "bravo", "charlie", "delta", "echo", "foxtrot", and "golf"`)
	require.Equal(t, []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}, terms)
}

func TestGatherExploreQuotedContentCandidatesUsesBoundedWidePages(t *testing.T) {
	terms := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot"}
	hits := make(map[string][]graph.ContentHit, len(terms))
	for _, term := range terms {
		hits[term] = quotedRecallHits(term, 1, 0)
	}
	server, store := newQuotedRecallCountingServer(t, hits)
	candidates := server.gatherExploreQuotedContentCandidates(
		context.Background(), `find "alpha", "bravo", "charlie", "delta", "echo", and "foxtrot"`, nil, 72,
		query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
	)

	require.Len(t, candidates, len(terms))
	require.Equal(t, []int{24, 24, 24, 24, 24, 24}, store.searchLimits)
	require.Equal(t, 1, store.graphLookups, "all term pages must share one graph lookup")
}

func TestGatherExploreContentCandidatesForBareTermsUsesSharedBounds(t *testing.T) {
	hits := map[string][]graph.ContentHit{
		"DISKFULL": {{
			NodeID: "demo/storage.go::persist", FilePath: "demo/storage.go",
			Snippet: `return errors.New("DISKFULL")`,
		}},
	}
	server, store := newQuotedRecallCountingServer(t, hits)
	candidates := server.gatherExploreContentCandidatesForTerms(
		context.Background(), "the daemon emits DISKFULL while persisting", []string{"DISKFULL"}, nil, 72,
		query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
	)

	require.Equal(t, []int{exploreQuotedRecallMaxPerTerm}, store.searchLimits)
	require.Equal(t, 1, store.graphLookups)
	exact := candidateByID(candidates, "demo/storage.go::persist")
	require.NotNil(t, exact)
	require.Equal(t, float64(1), exact.Signals[exploreContentRecallExactSignal])
}

func TestGatherExploreContentCandidatesForBareTermsCapsSearchesAndHydratesOnce(t *testing.T) {
	terms := []string{"ALPHA", "BRAVO", "CHARLIE", "DELTA", "ECHO"}
	hits := make(map[string][]graph.ContentHit, len(terms))
	for _, term := range terms {
		hits[term] = quotedRecallHits(term, exploreQuotedRecallMaxPerTerm, -1)
	}
	server, store := newQuotedRecallCountingServer(t, hits)
	candidates := server.gatherExploreContentCandidatesForTerms(
		context.Background(), strings.Join(terms, " "), terms, nil, 72,
		query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
	)

	require.NotEmpty(t, candidates)
	require.LessOrEqual(t, len(store.searchLimits), exploreBareLiteralMaxTerms+1)
	require.Equal(t, 1, store.graphLookups, "all bare term pages must share one graph lookup")
}

func TestGatherExploreQuotedContentCandidatesRetriesOneSaturatedTermWithinBounds(t *testing.T) {
	hits := map[string][]graph.ContentHit{
		"ku": quotedRecallHits("ku", exploreQuotedRecallRetryMaxRows, 17),
		"aa": quotedRecallHits("aa", exploreQuotedRecallMaxPerTerm, -1),
		"zz": quotedRecallHits("zz", exploreQuotedRecallMaxPerTerm, -1),
	}
	server, store := newQuotedRecallCountingServer(t, hits)
	candidates := server.gatherExploreQuotedContentCandidates(
		context.Background(), `find registration for "ku", "aa", and "zz"`, nil, 20,
		query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
	)

	require.LessOrEqual(t, len(store.searchLimits), exploreQuotedRecallMaxTerms+1)
	require.Equal(t, []int{6, 6, 6, exploreQuotedRecallRetryMaxRows}, store.searchLimits)
	require.Equal(t, []int{6, 6, 6, exploreQuotedRecallRetryMaxRows}, store.searchRows)
	require.LessOrEqual(t, store.searchRows[len(store.searchRows)-1], exploreQuotedRecallRetryMaxRows)
	require.Equal(t, 1, store.graphLookups, "all final pages must share one graph lookup")

	exactID := "demo/ku_17.go::candidate"
	exact := candidateByID(candidates, exactID)
	require.NotNil(t, exact, "bounded retry must recover a short exact literal beyond the first page")
	require.Equal(t, float64(1), exact.Signals[exploreContentRecallTermSignal], "retry replacement must not double-count a term")
	require.Equal(t, float64(1), exact.Signals[exploreContentRecallExactSignal])
	require.Equal(t, float64(1), exact.Signals[exploreContentRecallAmbiguousSignal], "a saturated final page remains ambiguous")
}

func TestGatherExploreQuotedContentCandidatesKeepsUniqueExactFastPath(t *testing.T) {
	hits := map[string][]graph.ContentHit{
		"日本": {
			{NodeID: "demo/prefix.go::candidate", FilePath: "demo/prefix.go", Snippet: "日本語 formatter"},
			{NodeID: "demo/exact.go::candidate", FilePath: "demo/exact.go", Snippet: `supported["日本"] = formatter`},
		},
	}
	server, store := newQuotedRecallCountingServer(t, hits)
	candidates := server.gatherExploreQuotedContentCandidates(
		context.Background(), `find locale "日本"`, nil, 16,
		query.QueryOptions{RepoAllow: map[string]bool{"demo": true}},
	)

	require.Equal(t, []int{5}, store.searchLimits)
	require.Equal(t, 1, store.graphLookups)
	exact := candidateByID(candidates, "demo/exact.go::candidate")
	require.NotNil(t, exact)
	require.Equal(t, float64(1), exact.Signals[exploreContentRecallExactSignal])
	require.Zero(t, exact.Signals[exploreContentRecallAmbiguousSignal])
}

func TestMergeExploreCandidatesRetainsExactAmbiguitySignal(t *testing.T) {
	node := &graph.Node{ID: "same", Name: "candidate", Kind: graph.KindFunction, FilePath: "candidate.go"}
	merged := mergeExploreCandidates(
		[]*rerank.Candidate{{Node: node, TextRank: 0, VectorRank: -1}},
		[]*rerank.Candidate{{Node: node, TextRank: 1, VectorRank: -1, Signals: map[string]float64{
			exploreContentRecallExactSignal: 1, exploreContentRecallAmbiguousSignal: 1,
		}}},
		0,
	)
	require.Len(t, merged, 1)
	require.Equal(t, float64(1), merged[0].Signals[exploreContentRecallAmbiguousSignal])
}

func quotedReadyNode(id, evidence string) *graph.Node {
	return &graph.Node{
		ID: id, Name: "candidate", QualName: evidence, Kind: graph.KindFunction,
		FilePath: id + ".go",
	}
}

func TestRerankExploreConceptCoveragePromotesDominantAmbiguousExactPeer(t *testing.T) {
	ambiguous := func(id, doc string, terms, rank float64) *rerank.Candidate {
		return &rerank.Candidate{
			Node:     &graph.Node{ID: id, Name: "candidate", Kind: graph.KindFunction, FilePath: id + ".go", Meta: map[string]any{"doc": doc}},
			TextRank: 0, VectorRank: -1,
			Signals: map[string]float64{
				exploreContentRecallExactSignal: 1, exploreContentRecallAmbiguousSignal: 1,
				exploreContentRecallTermSignal: terms, exploreContentRecallRankSignal: rank,
			},
		}
	}
	weak := ambiguous("weak", "locale", 3, 1)
	dominant := ambiguous("dominant", "locale registry pipeline", 1, 0.1)
	got := rerankExploreConceptCoverage("find locale registry pipeline behavior", []*rerank.Candidate{weak, dominant})
	require.Same(t, dominant, got[0], "query coverage must beat stronger content rank between ambiguous exact peers")
}

func TestRerankExploreConceptCoveragePrefersUniqueExactOverAmbiguous(t *testing.T) {
	ambiguous := &rerank.Candidate{
		Node:     &graph.Node{ID: "ambiguous", Name: "candidate", Kind: graph.KindFunction, FilePath: "ambiguous.go", Meta: map[string]any{"doc": "locale registry pipeline"}},
		TextRank: 0, VectorRank: -1,
		Signals: map[string]float64{exploreContentRecallExactSignal: 1, exploreContentRecallAmbiguousSignal: 1},
	}
	unique := &rerank.Candidate{
		Node:     &graph.Node{ID: "unique", Name: "candidate", Kind: graph.KindFunction, FilePath: "unique.go"},
		TextRank: 1, VectorRank: -1,
		Signals: map[string]float64{exploreContentRecallExactSignal: 1},
	}
	got := rerankExploreConceptCoverage("find locale registry pipeline behavior", []*rerank.Candidate{ambiguous, unique})
	require.Same(t, unique, got[0])
}

func TestExploreAnswerReadyDistinguishesUniqueAndAmbiguousExactPeers(t *testing.T) {
	t.Run("unique exact remains immediate", func(t *testing.T) {
		head := exploreTarget{node: quotedReadyNode("unique", "unrelated implementation"), exactContent: true}
		require.True(t, exploreAnswerReady(`unsupported locale "ku"`, []exploreTarget{head}))
	})

	t.Run("tied exact peers do not fall through lexical readiness", func(t *testing.T) {
		head := exploreTarget{node: quotedReadyNode("head", "locale registry"), exactContent: true, exactContentAmbiguous: true}
		peer := exploreTarget{node: quotedReadyNode("peer", "locale registry"), exactContent: true}
		require.False(t, exploreAnswerReady(`find locale registry for "ku"`, []exploreTarget{head, peer}))
	})

	t.Run("strictly dominant exact head is terminal", func(t *testing.T) {
		head := exploreTarget{node: quotedReadyNode("head", "locale registry pipeline"), exactContent: true, exactContentAmbiguous: true}
		peer := exploreTarget{node: quotedReadyNode("peer", "locale registry"), exactContent: true}
		require.True(t, exploreAnswerReady(`find locale registry pipeline for "ku"`, []exploreTarget{head, peer}))
	})

	t.Run("one non literal anchor plus structural alignment is terminal", func(t *testing.T) {
		head := exploreTarget{
			node: quotedReadyNode("head", "locale"), exactContent: true, exactContentAmbiguous: true,
			callers: []*graph.Node{quotedReadyNode("caller", "registration pipeline")},
		}
		peer := exploreTarget{node: quotedReadyNode("peer", "handler"), exactContent: true}
		require.True(t, exploreAnswerReady(`find locale registration pipeline for "ku"`, []exploreTarget{head, peer}))
	})
}

func TestExploreLocalizationExactTargetRequiresTwoTermsPerConceptCandidate(t *testing.T) {
	targets := []exploreTarget{
		{node: quotedReadyNode("one", "alpha handler")},
		{node: quotedReadyNode("two", "beta handler")},
	}
	require.Empty(t, exploreLocalizationExactTarget("find how alpha and beta cooperate", targets))
}

func BenchmarkExploreAnswerReadyAmbiguousExactPeers(b *testing.B) {
	head := exploreTarget{node: quotedReadyNode("head", "locale registry pipeline"), exactContent: true, exactContentAmbiguous: true}
	peer := exploreTarget{node: quotedReadyNode("peer", "locale registry"), exactContent: true}
	targets := []exploreTarget{head, peer}
	task := `find locale registry pipeline for "ku"`
	require.True(b, exploreAnswerReady(task, targets))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		exploreAnswerReady(task, targets)
	}
}
