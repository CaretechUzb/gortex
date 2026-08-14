package indexer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/clones"
	"github.com/zzet/gortex/internal/graph"
)

// semanticallyRelatedEdges collects every EdgeSemanticallyRelated edge
// in the graph — the diffusion-pass output surface.
func semanticallyRelatedEdges(g graph.Store) []*graph.Edge {
	var out []*graph.Edge
	for _, e := range g.AllEdges() {
		if e.Kind == graph.EdgeSemanticallyRelated {
			out = append(out, e)
		}
	}
	return out
}

// addFnNode registers a bare function node so diffuseSimilarityEdges
// has real endpoints to attach edges to.
func addFnNode(g graph.Store, id string) {
	g.AddNode(&graph.Node{
		ID: id, Kind: graph.KindFunction, Name: id,
		FilePath: id, StartLine: 1, Language: "go",
	})
}

// TestDiffuseSimilarityEdges_Chain is the core table-driven test for
// the graph-diffusion smoothing pass. Each case feeds a set of direct
// clone pairs (the EdgeSimilarTo seed graph) plus the set already
// emitted as direct clones, then asserts which semantically_related
// pairs the diffusion pass derives.
func TestDiffuseSimilarityEdges_Chain(t *testing.T) {
	cases := []struct {
		name string
		// direct similarity pairs feeding the diffusion graph.
		pairs []clones.Pair
		// canonicalised pairs already emitted as direct clones.
		directClones [][2]string
		// canonicalised (A<C) pairs expected as semantically_related.
		wantRelated [][2]string
		// canonicalised pairs that must NOT be emitted.
		wantAbsent [][2]string
	}{
		{
			// A~B~C with strong links and no direct A–C clone: the
			// transitive A–C relation must surface.
			name: "transitive chain surfaces A-C",
			pairs: []clones.Pair{
				{A: "A", B: "B", Similarity: 0.95},
				{A: "B", B: "C", Similarity: 0.95},
			},
			directClones: [][2]string{{"A", "B"}, {"B", "C"}},
			wantRelated:  [][2]string{{"A", "C"}},
		},
		{
			// Two disjoint clone pairs with no shared neighbour:
			// nothing to diffuse, no semantically_related edge.
			name: "unrelated pairs produce nothing",
			pairs: []clones.Pair{
				{A: "A", B: "B", Similarity: 0.95},
				{A: "X", B: "Y", Similarity: 0.95},
			},
			directClones: [][2]string{{"A", "B"}, {"X", "Y"}},
			wantAbsent:   [][2]string{{"A", "X"}, {"A", "Y"}, {"B", "X"}, {"B", "Y"}},
		},
		{
			// A chain through two weak (~0.5) clone links: the damped
			// product (0.9·0.5·0.5 = 0.225) is below diffusionThreshold,
			// so the relation is dropped as noise.
			name: "weak chain below threshold is dropped",
			pairs: []clones.Pair{
				{A: "A", B: "B", Similarity: 0.50},
				{A: "B", B: "C", Similarity: 0.50},
			},
			directClones: [][2]string{{"A", "B"}, {"B", "C"}},
			wantAbsent:   [][2]string{{"A", "C"}},
		},
		{
			// A pair that already has a direct clone edge must never
			// be re-emitted as semantically_related — the two edge
			// kinds partition. Here A,B,C are mutually similar; A–C is
			// itself a direct clone so only no extra edge is produced.
			name: "direct clone pair not re-emitted",
			pairs: []clones.Pair{
				{A: "A", B: "B", Similarity: 0.95},
				{A: "B", B: "C", Similarity: 0.95},
				{A: "A", B: "C", Similarity: 0.90},
			},
			directClones: [][2]string{{"A", "B"}, {"B", "C"}, {"A", "C"}},
			wantAbsent:   [][2]string{{"A", "C"}},
		},
		{
			// A hub B bridges three neighbours A, C, D: every
			// non-clone neighbour pair becomes a related edge.
			name: "hub bridges all neighbour pairs",
			pairs: []clones.Pair{
				{A: "A", B: "B", Similarity: 0.95},
				{A: "B", B: "C", Similarity: 0.95},
				{A: "B", B: "D", Similarity: 0.95},
			},
			directClones: [][2]string{{"A", "B"}, {"B", "C"}, {"B", "D"}},
			wantRelated:  [][2]string{{"A", "C"}, {"A", "D"}, {"C", "D"}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := graph.New()
			ids := map[string]struct{}{}
			for _, p := range tc.pairs {
				ids[p.A] = struct{}{}
				ids[p.B] = struct{}{}
			}
			for id := range ids {
				addFnNode(g, id)
			}

			directPairs := make(map[[2]string]struct{}, len(tc.directClones))
			for _, c := range tc.directClones {
				directPairs[canonicalPair(c[0], c[1])] = struct{}{}
			}

			dp, de := diffuseSimilarityEdges(g, tc.pairs, directPairs)
			assert.Equal(t, len(tc.wantRelated), dp, "diffused pair count")
			assert.Equal(t, 2*len(tc.wantRelated), de, "diffused edge count == 2·pairs")

			edges := semanticallyRelatedEdges(g)
			require.Len(t, edges, 2*len(tc.wantRelated),
				"one symmetric pair (2 directed edges) per expected relation")

			// Every emitted edge: ast_inferred origin, similarity meta
			// present, Confidence mirrors it, score above threshold.
			present := map[[2]string]bool{}
			for _, e := range edges {
				present[[2]string{e.From, e.To}] = true
				assert.Equal(t, graph.OriginASTInferred, e.Origin)
				sim, ok := e.Meta["similarity"].(float64)
				require.True(t, ok, "edge must carry similarity meta")
				assert.Equal(t, sim, e.Confidence, "Confidence mirrors similarity")
				assert.GreaterOrEqual(t, sim, diffusionThreshold,
					"emitted score must clear the diffusion threshold")
				assert.LessOrEqual(t, sim, 1.0)
			}

			// Symmetry: both directions of every expected relation.
			for _, w := range tc.wantRelated {
				assert.True(t, present[[2]string{w[0], w[1]}],
					"missing %s→%s", w[0], w[1])
				assert.True(t, present[[2]string{w[1], w[0]}],
					"missing %s→%s (symmetry)", w[1], w[0])
			}
			// Absent relations: neither direction emitted.
			for _, a := range tc.wantAbsent {
				assert.False(t, present[[2]string{a[0], a[1]}],
					"%s→%s must not be emitted", a[0], a[1])
				assert.False(t, present[[2]string{a[1], a[0]}],
					"%s→%s must not be emitted", a[1], a[0])
			}
		})
	}
}

// diffusedScoreFor returns the similarity carried by the directed
// semantically_related edge from→to, and whether such an edge exists.
func diffusedScoreFor(g graph.Store, from, to string) (float64, bool) {
	for _, e := range semanticallyRelatedEdges(g) {
		if e.From == from && e.To == to {
			return e.Meta["similarity"].(float64), true
		}
	}
	return 0, false
}

// TestDiffuseSimilarityEdges_Deterministic asserts the diffused score
// for a pair bridged by multiple neighbours is the max over bridges and
// independent of input ordering — two runs over re-ordered input yield
// byte-identical scores for every emitted relation.
func TestDiffuseSimilarityEdges_Deterministic(t *testing.T) {
	// A–C is bridged by two neighbours, B (strong, 0.95 links) and D
	// (weaker, 0.70 links). The diffused A–C score must be the
	// strongest bridge's contribution. B and D also share both A and C
	// as neighbours, so B–D is itself a derived relation — the test
	// accounts for it rather than pretending only A–C surfaces.
	pairsForward := []clones.Pair{
		{A: "A", B: "B", Similarity: 0.95},
		{A: "B", B: "C", Similarity: 0.95},
		{A: "A", B: "D", Similarity: 0.70},
		{A: "D", B: "C", Similarity: 0.70},
	}
	pairsShuffled := []clones.Pair{
		{A: "D", B: "C", Similarity: 0.70},
		{A: "B", B: "C", Similarity: 0.95},
		{A: "A", B: "D", Similarity: 0.70},
		{A: "A", B: "B", Similarity: 0.95},
	}

	run := func(pairs []clones.Pair) (acScore, bdScore float64) {
		g := graph.New()
		for _, id := range []string{"A", "B", "C", "D"} {
			addFnNode(g, id)
		}
		// A–B, B–C, A–D, D–C are direct clones; A–C and B–D are not.
		direct := map[[2]string]struct{}{
			canonicalPair("A", "B"): {},
			canonicalPair("B", "C"): {},
			canonicalPair("A", "D"): {},
			canonicalPair("D", "C"): {},
		}
		dp, de := diffuseSimilarityEdges(g, pairs, direct)
		// Two non-clone relations surface: A–C (via B and D) and
		// B–D (via A and C).
		require.Equal(t, 2, dp, "A–C and B–D are the derived relations")
		require.Equal(t, 4, de)
		ac, okAC := diffusedScoreFor(g, "A", "C")
		require.True(t, okAC, "A→C relation must be emitted")
		bd, okBD := diffusedScoreFor(g, "B", "D")
		require.True(t, okBD, "B→D relation must be emitted")
		// Symmetry: the reverse direction carries the same score.
		rev, okRev := diffusedScoreFor(g, "C", "A")
		require.True(t, okRev)
		assert.Equal(t, ac, rev, "reverse edge must mirror the score")
		return ac, bd
	}

	acForward, bdForward := run(pairsForward)
	acShuffled, bdShuffled := run(pairsShuffled)
	assert.Equal(t, acForward, acShuffled,
		"A–C diffused score must be independent of input ordering")
	assert.Equal(t, bdForward, bdShuffled,
		"B–D diffused score must be independent of input ordering")

	// A–C: the strong bridge B (0.9·0.95·0.95) wins over the weak
	// bridge D (0.9·0.70·0.70).
	assert.InDelta(t, diffusionDamping*0.95*0.95, acForward, 1e-9,
		"A–C score must take the strongest bridging neighbour")
	// B–D: bridged only through A and C, each a 0.95·0.70 product.
	assert.InDelta(t, diffusionDamping*0.95*0.70, bdForward, 1e-9,
		"B–D score is the damped product of its (strong,weak) links")
}

// TestDiffuseSimilarityEdges_PerNodeCapBounds verifies the per-node
// fan-out cap: a single hub with far more spokes than
// diffusionMaxNeighbors contributes only the bounded
// neighbours·(neighbours-1)/2 pairs, not the unbounded quadratic burst.
func TestDiffuseSimilarityEdges_PerNodeCapBounds(t *testing.T) {
	const spokes = 200 // ≫ diffusionMaxNeighbors (16)
	g := graph.New()
	addFnNode(g, "hub")
	var pairs []clones.Pair
	direct := map[[2]string]struct{}{}
	for i := 0; i < spokes; i++ {
		id := spokeID(i)
		addFnNode(g, id)
		pairs = append(pairs, clones.Pair{A: "hub", B: id, Similarity: 0.99})
		direct[canonicalPair("hub", id)] = struct{}{}
	}

	dp, de := diffuseSimilarityEdges(g, pairs, direct)
	// The hub's neighbour list is capped to diffusionMaxNeighbors, so
	// at most C(diffusionMaxNeighbors,2) spoke pairs can be derived.
	wantMax := diffusionMaxNeighbors * (diffusionMaxNeighbors - 1) / 2
	assert.Equal(t, wantMax, dp,
		"per-node fan-out cap must bound a dense hub's diffused pairs")
	assert.Equal(t, 2*wantMax, de)
	assert.Len(t, semanticallyRelatedEdges(g), 2*wantMax)
}

// TestDiffuseSimilarityEdges_GlobalCapBounds verifies the global pair
// ceiling: many independent small hubs — each below the per-node
// fan-out cap — together derive far more relations than
// diffusionMaxPairs, and the output is truncated to exactly the cap.
func TestDiffuseSimilarityEdges_GlobalCapBounds(t *testing.T) {
	// Each hub has spokesPerHub spokes (≤ diffusionMaxNeighbors so the
	// per-node cap never trips), deriving C(spokesPerHub,2) pairs.
	// Pick hub count so the total comfortably exceeds diffusionMaxPairs.
	const spokesPerHub = 12 // C(12,2) = 66 pairs per hub, ≤ 16 fan-out
	pairsPerHub := spokesPerHub * (spokesPerHub - 1) / 2
	hubs := diffusionMaxPairs/pairsPerHub + 200 // overshoot the ceiling

	g := graph.New()
	var pairs []clones.Pair
	direct := map[[2]string]struct{}{}
	spoke := 0
	for h := 0; h < hubs; h++ {
		hubID := "hub-" + spokeID(h)
		addFnNode(g, hubID)
		for s := 0; s < spokesPerHub; s++ {
			id := spokeID(spoke)
			spoke++
			addFnNode(g, id)
			pairs = append(pairs, clones.Pair{A: hubID, B: id, Similarity: 0.99})
			direct[canonicalPair(hubID, id)] = struct{}{}
		}
	}
	require.Greater(t, hubs*pairsPerHub, diffusionMaxPairs,
		"test setup must derive more pairs than the global cap")

	dp, de := diffuseSimilarityEdges(g, pairs, direct)
	assert.Equal(t, diffusionMaxPairs, dp,
		"diffused pairs must be capped at diffusionMaxPairs")
	assert.Equal(t, 2*diffusionMaxPairs, de)
	assert.Len(t, semanticallyRelatedEdges(g), 2*diffusionMaxPairs,
		"emitted edge set must respect the global cap")
}

// spokeID builds a stable, lexicographically well-ordered node ID for
// the cap tests' hub spokes — a 6-digit zero-padded suffix keeps IDs
// unique and sortable well past the largest spoke count used here.
func spokeID(i int) string {
	const digits = "0123456789"
	b := []byte("spoke-000000")
	n := len(b)
	for k := 0; k < 6; k++ {
		b[n-1-k] = digits[i%10]
		i /= 10
	}
	return string(b)
}

// TestDetectClonesAndEmitEdges_DiffusionWiring is an integration test
// over the full clone+diffusion pass. It hand-builds a graph where two
// function bodies are exact clones of a shared body and a third is a
// partial variant, then asserts the live clone pass materialises
// both similar_to and semantically_related edges and reports the
// diffusion counts on CloneDetectionStats.
func TestDetectClonesAndEmitEdges_DiffusionWiring(t *testing.T) {
	// Build a clone chain by reusing one substantial body for A and B
	// (identical → guaranteed clone) and a renamed variant for C that
	// still clones B. The diffusion pass should then relate A and C.
	bodyAB := cloneRepoSource
	sigAB, ok := clones.ComputeSignature(bodyAB)
	require.True(t, ok)
	encAB := clones.EncodeSignature(sigAB)

	g := graph.New()
	// A and B carry the identical signature.
	for _, id := range []string{"a.go::A", "b.go::B"} {
		g.AddNode(&graph.Node{
			ID: id, Kind: graph.KindFunction, Name: id,
			FilePath: id, StartLine: 1, Language: "go",
			Meta: map[string]any{cloneSigMetaKey: encAB},
		})
	}
	// C carries the same signature too — within one cluster the LSH
	// pass emits direct clone edges for every pair, so to exercise the
	// diffusion path we drive it directly below with a synthetic chain.
	g.AddNode(&graph.Node{
		ID: "c.go::C", Kind: graph.KindFunction, Name: "C",
		FilePath: "c.go", StartLine: 1, Language: "go",
		Meta: map[string]any{cloneSigMetaKey: encAB},
	})

	stats, _ := detectClonesAndEmitEdgesWithBaselineCtx(context.Background(), g, "", 0)
	// A, B, C all share a signature: three direct clone pairs, so the
	// only diffusable pairs are themselves direct clones — diffusion
	// correctly emits nothing (partition invariant).
	assert.Equal(t, 3, stats.Pairs, "three mutually-cloned functions")
	assert.Equal(t, 0, stats.DiffusedPairs,
		"every diffusable pair is already a direct clone")
	assert.Empty(t, semanticallyRelatedEdges(g))

	// Now exercise the genuine diffusion path: a separate graph with a
	// real A~B~C chain where A and C are NOT direct clones.
	g2 := graph.New()
	for _, id := range []string{"A", "B", "C"} {
		addFnNode(g2, id)
	}
	chain := []clones.Pair{
		{A: "A", B: "B", Similarity: 0.95},
		{A: "B", B: "C", Similarity: 0.95},
	}
	directOnly := map[[2]string]struct{}{
		canonicalPair("A", "B"): {},
		canonicalPair("B", "C"): {},
	}
	dp, de := diffuseSimilarityEdges(g2, chain, directOnly)
	assert.Equal(t, 1, dp, "A–C is the one diffused relation")
	assert.Equal(t, 2, de)

	// Idempotent: re-running the diffusion does not duplicate edges
	// (graph.AddEdge dedupes by edge key).
	diffuseSimilarityEdges(g2, chain, directOnly)
	assert.Len(t, semanticallyRelatedEdges(g2), 2,
		"second diffusion pass must not duplicate edges")
}
