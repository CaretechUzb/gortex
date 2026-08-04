package analysis

import (
	"fmt"
	"math"
	"testing"

	"github.com/zzet/gortex/internal/graph"
)

// pathGraph builds a directed path a0 -> a1 -> ... -> a(n-1) over
// EdgeCalls. On a path of length n the interior node at index i has
// an analytic betweenness of i * (n-1-i): every source at index < i
// reaches every target at index > i through it.
func pathGraph(n int) *graph.Graph {
	g := graph.New()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("p%d", i)
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
	}
	for i := 0; i < n-1; i++ {
		g.AddEdge(&graph.Edge{
			From: fmt.Sprintf("p%d", i),
			To:   fmt.Sprintf("p%d", i+1),
			Kind: graph.EdgeCalls,
		})
	}
	return g
}

// relayStar builds a directed star where every leaf calls the hub and
// the hub calls every leaf. The only path between two distinct leaves
// runs leaf -> hub -> leaf, so the hub's analytic betweenness is
// k*(k-1) for k leaves and every leaf scores 0.
func relayStar(leaves int) *graph.Graph {
	g := graph.New()
	g.AddNode(&graph.Node{ID: "hub", Kind: graph.KindFunction, Name: "hub"})
	for i := 0; i < leaves; i++ {
		id := fmt.Sprintf("leaf%d", i)
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
		g.AddEdge(&graph.Edge{From: id, To: "hub", Kind: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: "hub", To: id, Kind: graph.EdgeCalls})
	}
	return g
}

func TestComputeBetweenness_EmptyGraph(t *testing.T) {
	r := ComputeBetweenness(graph.New())
	if len(r.Scores) != 0 {
		t.Errorf("empty graph should yield no scores, got %d", len(r.Scores))
	}
	if r.Max != 0 {
		t.Errorf("Max = %f, want 0", r.Max)
	}
	if r.Sampled {
		t.Errorf("empty graph should not report Sampled")
	}
}

func TestComputeBetweenness_NilGraph(t *testing.T) {
	r := ComputeBetweenness(nil)
	if r == nil || len(r.Scores) != 0 {
		t.Fatalf("nil graph should yield an empty result, got %+v", r)
	}
}

// TestComputeBetweenness_ExactPathGraph checks exact Brandes' against
// the closed-form betweenness of a directed path. Every node's score
// is hand-checkable: index i on a path of n nodes scores i*(n-1-i).
func TestComputeBetweenness_ExactPathGraph(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want map[string]float64
	}{
		{
			name: "path of 5",
			n:    5,
			// p0,p4 are endpoints (0). p1: 1*3=3. p2: 2*2=4. p3: 3*1=3.
			want: map[string]float64{"p0": 0, "p1": 3, "p2": 4, "p3": 3, "p4": 0},
		},
		{
			name: "path of 4",
			n:    4,
			// p1: 1*2=2. p2: 2*1=2.
			want: map[string]float64{"p0": 0, "p1": 2, "p2": 2, "p3": 0},
		},
		{
			name: "path of 3",
			n:    3,
			// only p1 is interior: 1*1=1.
			want: map[string]float64{"p0": 0, "p1": 1, "p2": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ComputeBetweenness(pathGraph(tt.n))
			if r.Sampled {
				t.Fatalf("small graph (%d nodes) must use the exact path", tt.n)
			}
			if r.Pivots != tt.n {
				t.Errorf("exact path should run from every node: Pivots=%d, want %d", r.Pivots, tt.n)
			}
			for id, want := range tt.want {
				if got := r.ScoreOf(id); math.Abs(got-want) > 1e-9 {
					t.Errorf("betweenness(%s) = %v, want %v", id, got, want)
				}
			}
		})
	}
}

// TestComputeBetweenness_ExactStarGraph checks exact Brandes' against
// the closed-form betweenness of a relay star: the hub scores
// k*(k-1), every leaf scores 0.
func TestComputeBetweenness_ExactStarGraph(t *testing.T) {
	tests := []struct {
		name    string
		leaves  int
		wantHub float64
	}{
		{name: "3 leaves", leaves: 3, wantHub: 6},  // 3*2
		{name: "4 leaves", leaves: 4, wantHub: 12}, // 4*3
		{name: "6 leaves", leaves: 6, wantHub: 30}, // 6*5
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ComputeBetweenness(relayStar(tt.leaves))
			if r.Sampled {
				t.Fatalf("small graph must use the exact path")
			}
			if got := r.ScoreOf("hub"); math.Abs(got-tt.wantHub) > 1e-9 {
				t.Errorf("hub betweenness = %v, want %v", got, tt.wantHub)
			}
			if r.Max != r.ScoreOf("hub") {
				t.Errorf("hub should hold the max score: max=%v hub=%v", r.Max, r.ScoreOf("hub"))
			}
			for i := 0; i < tt.leaves; i++ {
				leaf := fmt.Sprintf("leaf%d", i)
				if got := r.ScoreOf(leaf); math.Abs(got) > 1e-9 {
					t.Errorf("leaf %s should have zero betweenness, got %v", leaf, got)
				}
			}
		})
	}
}

// TestComputeBetweenness_AdaptiveThreshold verifies the fast path
// switch: at or below betweennessExactThreshold every node is a
// source; above it the sampled path runs from a bounded pivot set.
func TestComputeBetweenness_AdaptiveThreshold(t *testing.T) {
	tests := []struct {
		name        string
		nodes       int
		wantSampled bool
		wantPivots  int
	}{
		{name: "below threshold stays exact", nodes: betweennessExactThreshold - 1, wantSampled: false, wantPivots: betweennessExactThreshold - 1},
		{name: "at threshold stays exact", nodes: betweennessExactThreshold, wantSampled: false, wantPivots: betweennessExactThreshold},
		{name: "above threshold goes sampled", nodes: betweennessExactThreshold + 1, wantSampled: true, wantPivots: betweennessPivots},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ComputeBetweenness(pathGraph(tt.nodes))
			if r.Sampled != tt.wantSampled {
				t.Errorf("Sampled = %v, want %v (nodes=%d)", r.Sampled, tt.wantSampled, tt.nodes)
			}
			if r.Pivots != tt.wantPivots {
				t.Errorf("Pivots = %d, want %d (nodes=%d)", r.Pivots, tt.wantPivots, tt.nodes)
			}
		})
	}
}

// TestComputeBetweenness_SampledApproximatesExact builds a graph past
// the exact threshold and checks the sampled estimate tracks the
// analytic betweenness of a long directed path. On a path the score
// of index i is i*(n-1-i); the sampled, V/k-rescaled estimate should
// land within a modest relative tolerance for the high-centrality
// interior nodes.
func TestComputeBetweenness_SampledApproximatesExact(t *testing.T) {
	n := betweennessExactThreshold + 1500
	g := pathGraph(n)

	r := ComputeBetweenness(g)
	if !r.Sampled {
		t.Fatalf("graph of %d nodes should use the sampled path", n)
	}

	// Check the middle of the path, where betweenness is largest and
	// the relative sampling error is smallest.
	mid := n / 2
	id := fmt.Sprintf("p%d", mid)
	want := float64(mid) * float64(n-1-mid)
	got := r.ScoreOf(id)

	relErr := math.Abs(got-want) / want
	const tolerance = 0.20 // 20% — a 256-pivot sample on a 3500-node path
	if relErr > tolerance {
		t.Errorf("sampled betweenness(%s) = %.0f, want ~%.0f (rel err %.3f > %.2f)",
			id, got, want, relErr, tolerance)
	}

	// The endpoints are never intermediate — they must stay at zero
	// regardless of which pivots were sampled.
	if got := r.ScoreOf("p0"); got != 0 {
		t.Errorf("path endpoint p0 betweenness = %v, want 0", got)
	}
	if got := r.ScoreOf(fmt.Sprintf("p%d", n-1)); got != 0 {
		t.Errorf("path endpoint p%d betweenness = %v, want 0", n-1, got)
	}
}

// TestComputeBetweenness_SampledIsDeterministic verifies the
// fixed-seed pivot sampling produces byte-identical scores across
// repeated runs on the same graph.
func TestComputeBetweenness_SampledIsDeterministic(t *testing.T) {
	n := betweennessExactThreshold + 800
	g := pathGraph(n)

	first := ComputeBetweenness(g)
	if !first.Sampled {
		t.Fatalf("graph of %d nodes should use the sampled path", n)
	}

	for run := 0; run < 5; run++ {
		again := ComputeBetweenness(g)
		if again.Pivots != first.Pivots {
			t.Fatalf("run %d: Pivots = %d, want %d", run, again.Pivots, first.Pivots)
		}
		if again.Max != first.Max {
			t.Errorf("run %d: Max = %v, want %v", run, again.Max, first.Max)
		}
		for id, want := range first.Scores {
			if got := again.Scores[id]; got != want {
				t.Errorf("run %d: score(%s) = %v, want %v — sampling not deterministic", run, id, got, want)
			}
		}
	}
}

// TestComputeBetweenness_LargeGraphCompletes builds a graph well past
// the exact threshold and asserts the sampled fast path returns a
// well-formed result. This exercises the O(k*E) structural fast path
// without a wall-clock bound.
func TestComputeBetweenness_LargeGraphCompletes(t *testing.T) {
	n := 12000
	g := graph.New()
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("n%d", i)
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
	}
	// A wide directed mesh: each node calls the next three. Plenty of
	// shortest paths cross the interior so betweenness is non-trivial.
	for i := 0; i < n; i++ {
		for d := 1; d <= 3 && i+d < n; d++ {
			g.AddEdge(&graph.Edge{
				From: fmt.Sprintf("n%d", i),
				To:   fmt.Sprintf("n%d", i+d),
				Kind: graph.EdgeCalls,
			})
		}
	}

	r := ComputeBetweenness(g)
	if !r.Sampled {
		t.Fatalf("graph of %d nodes should use the sampled path", n)
	}
	if r.Pivots != betweennessPivots {
		t.Errorf("Pivots = %d, want %d", r.Pivots, betweennessPivots)
	}
	if len(r.Scores) != n {
		t.Errorf("Scores should cover every node: got %d, want %d", len(r.Scores), n)
	}
	if r.Max <= 0 {
		t.Errorf("a connected mesh should have a positive max betweenness, got %v", r.Max)
	}
}

// TestComputeBetweenness_OnlyCallAndReferenceEdges verifies that
// structural edges are ignored — a path wired with EdgeDefines
// carries no betweenness.
func TestComputeBetweenness_OnlyCallAndReferenceEdges(t *testing.T) {
	g := graph.New()
	for _, id := range []string{"x", "y", "z"} {
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
	}
	g.AddEdge(&graph.Edge{From: "x", To: "y", Kind: graph.EdgeDefines})
	g.AddEdge(&graph.Edge{From: "y", To: "z", Kind: graph.EdgeDefines})

	r := ComputeBetweenness(g)
	if r.Max != 0 {
		t.Errorf("structural edges should carry no betweenness, max=%v", r.Max)
	}

	// References participate exactly like calls.
	g2 := graph.New()
	for _, id := range []string{"x", "y", "z"} {
		g2.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
	}
	g2.AddEdge(&graph.Edge{From: "x", To: "y", Kind: graph.EdgeReferences})
	g2.AddEdge(&graph.Edge{From: "y", To: "z", Kind: graph.EdgeReferences})
	r2 := ComputeBetweenness(g2)
	if got := r2.ScoreOf("y"); math.Abs(got-1) > 1e-9 {
		t.Errorf("reference-edge path: betweenness(y) = %v, want 1", got)
	}
}

// TestFindHotspots_BetweennessComponent verifies the hotspot scorer
// surfaces a pure bottleneck. The relay hub has modest fan-in/out
// relative to a separately wired high-fan-in node, but it sits on
// every leaf-to-leaf shortest path — its Betweenness field must be
// populated and non-zero, and it must rank as a hotspot.
func TestFindHotspots_BetweennessComponent(t *testing.T) {
	g := relayStar(8)
	// Pad with extra unrelated functions so the graph clears the
	// 10-symbol floor the MCP handler enforces.
	for i := 0; i < 6; i++ {
		id := fmt.Sprintf("extra%d", i)
		g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
	}

	communities := &CommunityResult{NodeToComm: map[string]string{}}
	result := FindHotspots(g, communities, 0)

	var hub *HotspotEntry
	for i := range result {
		if result[i].ID == "hub" {
			hub = &result[i]
			break
		}
	}
	if hub == nil {
		t.Fatalf("relay hub should be reported as a hotspot, got %d entries", len(result))
	}
	if hub.Betweenness <= 0 {
		t.Errorf("relay hub should carry a positive betweenness component, got %v", hub.Betweenness)
	}
	// The hub is the single highest-betweenness node, so its
	// normalized betweenness should be the 0-100 ceiling.
	if math.Abs(hub.Betweenness-100) > 0.01 {
		t.Errorf("relay hub betweenness = %v, want 100 (it holds the graph max)", hub.Betweenness)
	}
	// A leaf is never an intermediate vertex — if it surfaces at all
	// its betweenness component is zero.
	for i := range result {
		if result[i].ID == "leaf0" && result[i].Betweenness != 0 {
			t.Errorf("leaf0 betweenness = %v, want 0", result[i].Betweenness)
		}
	}
}

// TestFindHotspots_BetweennessRaisesRank verifies the betweenness
// term augments — not replaces — the legacy fan-in/out signal: adding
// a bottleneck role to a node strictly raises its complexity score.
func TestFindHotspots_BetweennessRaisesRank(t *testing.T) {
	// Baseline: a plain 3-hop chain bridge -> via -> sink, plus
	// padding to clear the symbol floor.
	build := func(withBottleneck bool) []HotspotEntry {
		g := graph.New()
		ids := []string{"src", "via", "sink"}
		for _, id := range ids {
			g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
		}
		g.AddEdge(&graph.Edge{From: "src", To: "via", Kind: graph.EdgeCalls})
		g.AddEdge(&graph.Edge{From: "via", To: "sink", Kind: graph.EdgeCalls})
		if withBottleneck {
			// Route extra callers and callees through `via` so it
			// becomes a genuine shortest-path bottleneck.
			for i := 0; i < 4; i++ {
				in := fmt.Sprintf("in%d", i)
				out := fmt.Sprintf("out%d", i)
				g.AddNode(&graph.Node{ID: in, Kind: graph.KindFunction, Name: in})
				g.AddNode(&graph.Node{ID: out, Kind: graph.KindFunction, Name: out})
				g.AddEdge(&graph.Edge{From: in, To: "via", Kind: graph.EdgeCalls})
				g.AddEdge(&graph.Edge{From: "via", To: out, Kind: graph.EdgeCalls})
			}
		}
		for i := 0; i < 8; i++ {
			id := fmt.Sprintf("pad%d", i)
			g.AddNode(&graph.Node{ID: id, Kind: graph.KindFunction, Name: id})
		}
		return FindHotspots(g, &CommunityResult{NodeToComm: map[string]string{}}, 0)
	}

	scoreOf := func(entries []HotspotEntry, id string) (float64, bool) {
		for _, e := range entries {
			if e.ID == id {
				return e.ComplexityScore, true
			}
		}
		return 0, false
	}

	withBottleneck := build(true)
	viaScore, ok := scoreOf(withBottleneck, "via")
	if !ok {
		t.Fatalf("bottleneck node `via` should be reported as a hotspot")
	}
	if viaScore <= 0 {
		t.Errorf("bottleneck node should have a positive complexity score, got %v", viaScore)
	}
	// The bottleneck node carries both fan-in/out and betweenness, so
	// it must outrank the inert padding functions.
	if padScore, ok := scoreOf(withBottleneck, "pad0"); ok && viaScore <= padScore {
		t.Errorf("bottleneck node (%.2f) should outrank inert padding (%.2f)", viaScore, padScore)
	}
}

func BenchmarkComputeBetweennessSampled(b *testing.B) {
	g := pathGraph(betweennessExactThreshold + 1500)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ComputeBetweenness(g)
	}
}
