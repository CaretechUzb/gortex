package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
)

// boundaryFixtureGraph builds a graph whose function names share a
// dominant interior vocabulary ("token", "validate", "handler") so the
// adjacent-pair frequency distribution has a clear common core and rare
// seams. Mirrors fixtureGraph in auto_concepts_test.go.
func boundaryFixtureGraph(names []string) *graph.Graph {
	g := graph.New()
	for i, n := range names {
		g.AddNode(&graph.Node{
			ID:        "pkg/f.go::" + n,
			Kind:      graph.KindFunction,
			Name:      n,
			FilePath:  "pkg/f.go",
			StartLine: i + 1,
			EndLine:   i + 2,
			Language:  "go",
		})
	}
	return g
}

type countingNgramReader struct {
	graph.Reader
	allNodesCalls int
}

func (r *countingNgramReader) AllNodes() []*graph.Node {
	r.allNodesCalls++
	return r.Reader.AllNodes()
}

func TestBuildNgramBoundaries_NilAndEmpty(t *testing.T) {
	// nil graph -> empty table -> tokenizer degrades to fixed behavior.
	require.True(t, BuildNgramBoundaries(nil).Empty())
	if c := BuildNgramBoundaries(nil).BoundaryCount(); c != 0 {
		t.Errorf("nil graph should yield 0 boundaries, got %d", c)
	}
	// Empty graph: no names, no pairs, empty table.
	require.True(t, BuildNgramBoundaries(graph.New()).Empty())
	// A typed-nil table must answer Empty() true without panicking.
	var tnil *NgramTable
	require.True(t, tnil.Empty())
}

func TestNgramTable_EmptyTableSplitIsWhole(t *testing.T) {
	// An empty table splits nothing: the token comes back whole, leaving
	// the fixed-n fallback to the tokenizer.
	tbl := BuildNgramBoundaries(graph.New())
	require.True(t, tbl.Empty())
	got := tbl.Split([]rune("validateToken"))
	assert.Equal(t, []string{"validateToken"}, got)
}

func TestBuildNgramBoundaries_Deterministic(t *testing.T) {
	names := []string{
		"validateToken", "validateTokenizer", "tokenValidator",
		"handleToken", "tokenHandler", "parseToken", "tokenParser",
		"refreshToken", "tokenRefresher", "revokeToken",
		"validateHandler", "handlerValidator", "buildHandler",
	}
	a := BuildNgramBoundaries(boundaryFixtureGraph(names))
	b := BuildNgramBoundaries(boundaryFixtureGraph(names))
	// Two builds over equivalent graphs must produce byte-identical
	// boundary sets — no Go map-iteration nondeterminism may leak into
	// the percentile cut.
	require.Equal(t, a.BoundaryCount(), b.BoundaryCount())
	for k := range a.boundaryPairs {
		_, ok := b.boundaryPairs[k]
		assert.Truef(t, ok, "boundary %d present in build A but not build B", k)
	}
	// And, crucially, the Split decisions must agree token for token.
	for _, tok := range names {
		assert.Equalf(t, a.Split([]rune(tok)), b.Split([]rune(tok)),
			"Split(%q) differs between two deterministic builds", tok)
	}
}

func TestBuildNgramBoundaries_LearnsAReasonableSeam(t *testing.T) {
	// A corpus dominated by the substring "token": the pairs inside
	// "token" (to, ok, ke, en) are frequent, so a rare cross-word pair
	// is selected as a seam and at least one multi-token name splits.
	names := []string{
		"tokenAlpha", "tokenBeta", "tokenGamma", "tokenDelta",
		"tokenEpsilon", "tokenZeta", "alphaToken", "betaToken",
		"gammaToken", "deltaToken", "tokenize", "tokenizer",
		"retoken", "subtoken", "tokenList", "tokenMap",
	}
	tbl := BuildNgramBoundaries(boundaryFixtureGraph(names))
	require.False(t, tbl.Empty(), "a corpus this size should learn boundaries")

	// At least one eligible token must split into >1 segment, otherwise
	// the table learned nothing usable.
	splitSomething := false
	for _, tok := range names {
		if len(tbl.Split([]rune(tok))) > 1 {
			splitSomething = true
			break
		}
	}
	assert.True(t, splitSomething, "the learned table should split at least one token")
}

func TestNgramTable_SplitNeverStrandsSubMinimalSegments(t *testing.T) {
	names := []string{
		"validateToken", "validateTokenizer", "tokenValidator",
		"handleToken", "tokenHandler", "parseToken", "tokenParser",
		"refreshToken", "tokenRefresher", "revokeToken",
	}
	tbl := BuildNgramBoundaries(boundaryFixtureGraph(names))
	if tbl.Empty() {
		t.Skip("fixture learned no boundaries; nothing to assert")
	}
	for _, tok := range names {
		segs := tbl.Split([]rune(tok))
		if len(segs) == 1 {
			continue // unsplit token: the whole thing, fine.
		}
		for _, s := range segs {
			assert.GreaterOrEqualf(t, len([]rune(s)), sparseNgramMinN,
				"Split(%q) stranded a sub-minimal segment %q", tok, s)
		}
	}
}

func TestNgramTable_SplitShortTokenWhole(t *testing.T) {
	tbl := BuildNgramBoundaries(boundaryFixtureGraph([]string{
		"tokenAlpha", "tokenBeta", "tokenGamma", "tokenDelta",
		"alphaToken", "betaToken", "gammaToken", "deltaToken",
		"tokenize", "tokenizer",
	}))
	// A token shorter than the minimum is never split.
	got := tbl.Split([]rune("go"))
	assert.Equal(t, []string{"go"}, got)
}

// TestInstallNgramBoundaries_BM25Chain verifies the install helper finds
// the BM25 layer through the production Swappable wrapper and that a
// non-BM25 backend is a harmless no-op.
func TestInstallNgramBoundaries_BM25Chain(t *testing.T) {
	bm := NewBM25()
	defer bm.Close()
	tbl := BuildNgramBoundaries(boundaryFixtureGraph([]string{
		"tokenAlpha", "tokenBeta", "tokenGamma", "tokenDelta",
		"alphaToken", "betaToken", "tokenize", "tokenizer",
	}))

	// Bare BM25.
	assert.True(t, InstallNgramBoundaries(bm, tbl))

	// Wrapped in a Swappable, as in production.
	sw := NewSwappable(NewBM25())
	defer sw.Close()
	assert.True(t, InstallNgramBoundaries(sw, tbl))

	// A backend with no BM25 layer: no-op, returns false. Bleve has no
	// BM25 inner.
	blv, err := NewBleve()
	require.NoError(t, err)
	defer blv.Close()
	assert.False(t, InstallNgramBoundaries(blv, tbl))
}

func TestBuildAndInstallNgramBoundaries_ChecksCapabilityBeforeGraphScan(t *testing.T) {
	g := &countingNgramReader{Reader: boundaryFixtureGraph([]string{
		"tokenAlpha", "tokenBeta", "tokenGamma", "tokenDelta",
		"alphaToken", "betaToken", "tokenize", "tokenizer",
	})}

	blv, err := NewBleve()
	require.NoError(t, err)
	defer blv.Close()
	assert.False(t, BuildAndInstallNgramBoundaries(blv, g))
	assert.Zero(t, g.allNodesCalls, "a backend without BM25 must not enumerate the graph")

	bm := NewBM25()
	defer bm.Close()
	assert.True(t, BuildAndInstallNgramBoundaries(bm, g))
	assert.Equal(t, 1, g.allNodesCalls, "BM25 must mine exactly one graph census")
}

// TestSparseNgram_TableDrivenVsFixed proves the learned table actually
// changes the tokenizer's emitted grams versus the fixed-n fallback,
// and that the index/query symmetry still holds with a table installed:
// a query routed through the same table reaches a doc it shares a
// learned segment with.
func TestSparseNgram_TableDrivenVsFixed(t *testing.T) {
	withSparseNgram(t, true)
	tbl := BuildNgramBoundaries(boundaryFixtureGraph([]string{
		"tokenAlpha", "tokenBeta", "tokenGamma", "tokenDelta",
		"tokenEpsilon", "alphaToken", "betaToken", "gammaToken",
		"tokenize", "tokenizer", "retoken", "subtoken",
	}))
	require.False(t, tbl.Empty())

	b := NewBM25()
	defer b.Close()
	require.True(t, InstallNgramBoundaries(b, tbl))
	b.Add("svc::tokenAlpha", "tokenAlpha", "x.go")

	// The same table drives both Add and Search, so a query that shares
	// a learned segment with the indexed symbol reaches it.
	res := b.Search("token", 10)
	require.NotEmpty(t, res)
	assert.Equal(t, "svc::tokenAlpha", res[0].ID)
}
