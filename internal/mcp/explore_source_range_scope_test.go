package mcp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/query"
	"github.com/zzet/gortex/internal/search/rerank"
)

func TestExploreSourceRangeScopedGraphPathAliases(t *testing.T) {
	const (
		rawPath      = "monolog/src/Monolog/Handler/FingersCrossedHandler.php"
		resolvedPath = "monolog-1800/monolog/src/Monolog/Handler/FingersCrossedHandler.php"
		scopedPath   = "monolog-1800/src/Monolog/Handler/FingersCrossedHandler.php"
	)
	scope := query.QueryOptions{RepoAllow: map[string]bool{"monolog-1800": true}}
	got := exploreSourceRangeScopedGraphPathAliases(rawPath, resolvedPath, "monolog-1800", scope)
	require.NotEmpty(t, got)
	require.Equal(t, resolvedPath, got[0], "a resolved exact path must remain authoritative")
	require.Contains(t, got, scopedPath)
	require.LessOrEqual(t, len(got), exploreSourceRangeMaxAliases)

	resolvedIndex := &fileSymbolIndex{}
	scopedIndex := &fileSymbolIndex{}
	require.Same(t, resolvedIndex, exploreSourceRangeIndex(map[string]*fileSymbolIndex{
		resolvedPath: resolvedIndex,
		scopedPath:   scopedIndex,
	}, got), "a synthesized scoped path must not displace an indexed resolved path")

	got = exploreSourceRangeScopedGraphPathAliases(
		"spdlog/include/spdlog/details/os-inl.h",
		"",
		"",
		query.QueryOptions{RepoAllow: map[string]bool{"spdlog-3488": true}},
	)
	require.NotEmpty(t, got)
	require.Equal(t, "spdlog-3488/include/spdlog/details/os-inl.h", got[0])

	require.Nil(t, exploreSourceRangeScopedGraphPathAliases(
		rawPath,
		"",
		"",
		query.QueryOptions{RepoAllow: map[string]bool{
			"monolog-1800": true,
			"monolog-1900": true,
		}},
	))
	require.Nil(t, exploreSourceRangeScopedGraphPathAliases(
		"other/src/x.go", "other-1900/src/x.go", "other-1900", scope,
	), "a path resolved to a foreign repo must not suffix-fallback into the allowed repo")
	unknownAliases := exploreSourceRangeScopedGraphPathAliases("other/src/x.go", "", "", scope)
	require.Equal(t, []string{"monolog-1800/other/src/x.go"}, unknownAliases)
	require.Nil(t, exploreSourceRangeIndex(map[string]*fileSymbolIndex{
		"monolog-1800/src/x.go": {},
	}, unknownAliases), "an unrecognized label must not be shortened into the allowed repo")
	unknownResolved := exploreSourceRangeScopedGraphPathAliases(
		"other/src/x.go", "monolog-1800/other/src/x.go", "monolog-1800", scope,
	)
	require.Equal(t, []string{"monolog-1800/other/src/x.go"}, unknownResolved)
	require.Nil(t, exploreSourceRangeIndex(map[string]*fileSymbolIndex{
		"monolog-1800/src/x.go": {},
	}, unknownResolved), "a resolved unknown label must retain only its exact spelling")

	for _, unsafe := range []string{`C:\other\src\x.go`, `\\server\share\x.go`, "../src/x.go", "other/../src/x.go", "monolog/../src/x.go"} {
		require.Nil(t, exploreSourceRangeScopedGraphPathAliases(unsafe, "", "", scope), unsafe)
	}
	for _, traversal := range []string{"other/../src/x.go", "monolog/../src/x.go"} {
		require.Nil(t, exploreSourceRangeScopedGraphPathAliases(
			traversal, "monolog-1800/src/x.go", "monolog-1800", scope,
		), traversal)
	}
	require.Equal(t, []string{"monolog-1800/src/x.go"}, exploreSourceRangeScopedGraphPathAliases(
		`C:\other\src\x.go`, "monolog-1800/src/x.go", "monolog-1800", scope,
	), "an absolute citation may retain only its resolved exact spelling")
}

func TestPromoteExploreSourceRangeCandidatesUsesSingleScopedRepoPrefix(t *testing.T) {
	const file = "monolog-1800/src/Monolog/Handler/FingersCrossedHandler.php"
	owner := &graph.Node{
		ID:         file + "::FingersCrossedHandler",
		Name:       "FingersCrossedHandler",
		QualName:   "Monolog.Handler.FingersCrossedHandler",
		Kind:       graph.KindType,
		FilePath:   file,
		RepoPrefix: "monolog-1800",
		StartLine:  1,
		EndLine:    240,
	}
	flush := &graph.Node{
		ID:         file + "::FingersCrossedHandler.flushBuffer",
		Name:       "flushBuffer",
		QualName:   "FingersCrossedHandler.flushBuffer",
		Kind:       graph.KindMethod,
		FilePath:   file,
		RepoPrefix: "monolog-1800",
		StartLine:  180,
		EndLine:    195,
	}
	g := graph.New()
	g.AddBatch([]*graph.Node{owner, flush}, nil)
	s := &Server{graph: g}
	ordinary := []*rerank.Candidate{{Node: &graph.Node{
		ID:   "monolog-1800/src/Utils.php::pcreLastErrorMessage",
		Name: "pcreLastErrorMessage", Kind: graph.KindMethod,
		FilePath: "monolog-1800/src/Utils.php", RepoPrefix: "monolog-1800",
	}}}
	scope := query.QueryOptions{RepoAllow: map[string]bool{"monolog-1800": true}}

	got := s.promoteExploreSourceRangeCandidates(
		context.Background(),
		"Investigate monolog/src/Monolog/Handler/FingersCrossedHandler.php Lines 185–187.",
		ordinary,
		scope,
	)
	require.Len(t, got, 2)
	require.Equal(t, flush.ID, got[0].Node.ID)
	require.Equal(t, float64(1), got[0].Signals[exploreSourceRangeSignal])
	require.Same(t, ordinary[0], got[1])

	ambiguous := s.promoteExploreSourceRangeCandidates(
		context.Background(),
		"Investigate monolog/src/Monolog/Handler/FingersCrossedHandler.php Lines 185–187.",
		ordinary,
		query.QueryOptions{RepoAllow: map[string]bool{
			"monolog-1800": true,
			"monolog-1900": true,
		}},
	)
	require.Len(t, ambiguous, 1)
	require.Same(t, ordinary[0], ambiguous[0])

	outOfScope := s.promoteExploreSourceRangeCandidates(
		context.Background(),
		"Investigate monolog/src/Monolog/Handler/FingersCrossedHandler.php Lines 185–187.",
		ordinary,
		query.QueryOptions{RepoAllow: map[string]bool{"monolog-1900": true}},
	)
	require.Len(t, outOfScope, 1)
	require.Same(t, ordinary[0], outOfScope[0])

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	canceled := s.promoteExploreSourceRangeCandidates(
		canceledCtx,
		"Investigate monolog/src/Monolog/Handler/FingersCrossedHandler.php Lines 185–187.",
		ordinary,
		scope,
	)
	require.Len(t, canceled, 1)
	require.Same(t, ordinary[0], canceled[0])
}
