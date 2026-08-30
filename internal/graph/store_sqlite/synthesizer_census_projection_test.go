package store_sqlite

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zzet/gortex/internal/graph"
)

// Every edge kind observed carrying a synthesizer stamp on a real store, plus
// one the projection has never been told about. The census must not depend on
// this list — it is the fixture, not the contract.
var censusStampedKinds = []graph.EdgeKind{
	graph.EdgeReferences,
	graph.EdgeExtends,
	graph.EdgeImports,
	graph.EdgeReads,
	graph.EdgeComposes,
	graph.EdgeOverrides,
	graph.EdgeCalls,
	graph.EdgeRendersChild,
}

func censusNode(name string) *graph.Node {
	return &graph.Node{
		ID: "repoA/a.go::" + name, Kind: graph.KindFunction,
		FilePath: "repoA/a.go", RepoPrefix: "repoA",
	}
}

func stampedEdge(kind graph.EdgeKind, line int, by string) *graph.Edge {
	return &graph.Edge{
		From: "repoA/a.go::From", To: "repoA/a.go::To", Kind: kind,
		FilePath: "repoA/a.go", Line: line,
		Meta: map[string]any{"synthesized_by": by, "provenance": "heuristic", "via": "v"},
	}
}

func collectCensus(t *testing.T, s graph.Store) map[graph.EdgeKind]int {
	t.Helper()
	seq, scanErr := graph.SynthesizedEdgesSeq(s)
	out := map[graph.EdgeKind]int{}
	for row := range seq {
		out[row.Kind]++
	}
	require.NoError(t, scanErr())
	return out
}

// The defect this projection exists to avoid: FrameworkCensusEdgesSeq withholds
// SynthesizedBy from every kind but references and imports, so reusing it would
// have reported ~98% of the edges and called it a whole-graph census. Six of
// these eight kinds are invisible through that projection.
func TestSynthesizerCensusCountsEveryKindThatCarriesAStamp(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{censusNode("From"), censusNode("To")}, nil)

	var edges []*graph.Edge
	for i, kind := range censusStampedKinds {
		edges = append(edges, stampedEdge(kind, i+1, "odoo"))
	}
	store.AddBatch(nil, edges)

	byKind := collectCensus(t, store)
	require.Len(t, byKind, len(censusStampedKinds), "every stamped kind must appear: got %v", byKind)
	for _, kind := range censusStampedKinds {
		require.Equal(t, 1, byKind[kind], "kind %q was dropped from the census", kind)
	}
}

// Pins the "no hard-coded kind list" decision. A synthesizer that starts
// stamping a kind nobody enumerated must still be counted, or this tool goes
// back to under-reporting silently — the exact failure it was fixed for.
func TestSynthesizerCensusCountsAKindItWasNeverToldAbout(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{censusNode("From"), censusNode("To")}, nil)
	store.AddBatch(nil, []*graph.Edge{stampedEdge(graph.EdgeMemberOf, 1, "brand-new-pass")})

	require.Equal(t, 1, collectCensus(t, store)[graph.EdgeMemberOf],
		"an unenumerated kind must still be counted")
}

// The whole point. panicOnFatal deliberately swallows a closed store as a
// benign teardown race, so the scan ends exactly like a clean exhaust. Without
// the recorded error the caller publishes an empty census as a real answer.
func TestSynthesizerCensusReportsAnAbortedScan(t *testing.T) {
	t.Parallel()
	store, err := Open(filepath.Join(t.TempDir(), "abort.sqlite"))
	require.NoError(t, err)
	store.AddBatch([]*graph.Node{censusNode("From"), censusNode("To")}, nil)
	store.AddBatch(nil, []*graph.Edge{stampedEdge(graph.EdgeReferences, 1, "odoo")})
	require.NoError(t, store.Close())

	seq, scanErr := store.SynthesizedEdgesSeq()
	rows := 0
	for range seq {
		rows++
	}
	require.Error(t, scanErr(), "a read against a closed store must not look like an empty graph")
	require.Zero(t, rows)
}

// The probe is a prefilter, not the decision: a blob carrying the key bytes in
// a VALUE has no stamp, and only the decode can tell.
func TestSynthesizerCensusRejectsAProbeFalsePositive(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	store.AddBatch([]*graph.Node{censusNode("From"), censusNode("To")}, nil)
	store.AddBatch(nil, []*graph.Edge{{
		From: "repoA/a.go::From", To: "repoA/a.go::To", Kind: graph.EdgeReferences,
		FilePath: "repoA/a.go", Line: 1,
		Meta: map[string]any{"via": "synthesized_by"},
	}})

	require.Empty(t, collectCensus(t, store), "the key bytes inside a value are not a stamp")
}

// The streaming projection and the in-memory fallback must be the same census.
// If they can disagree, the fallback stops being a correctness path.
func TestSynthesizerCensusMatchesTheInMemoryFallback(t *testing.T) {
	t.Parallel()
	store := openGenTestStore(t)
	mem := graph.New()
	store.AddBatch([]*graph.Node{censusNode("From"), censusNode("To")}, nil)

	names := []string{"odoo", "grpc-stub", "odoo"}
	for i, kind := range censusStampedKinds[:3] {
		edge := stampedEdge(kind, i+1, names[i])
		store.AddBatch(nil, []*graph.Edge{edge})
		mem.AddEdge(edge)
	}

	describe := func(s graph.Store) []string {
		seq, scanErr := graph.SynthesizedEdgesSeq(s)
		var got []string
		for row := range seq {
			got = append(got, string(row.Kind)+"/"+row.SynthesizedBy+"/"+row.Provenance+"/"+row.Via)
		}
		require.NoError(t, scanErr())
		sort.Strings(got)
		return got
	}
	require.Equal(t, describe(mem), describe(store))
}
