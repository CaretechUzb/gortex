package resolver

import (
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zzet/gortex/internal/graph"
	"github.com/zzet/gortex/internal/graph/store_sqlite"
)

type attributionPagingProbe struct {
	graph.Store
	scanner     graph.UnresolvedEdgeIdentityBatchScanner
	finder      graph.EdgeIdentityBatchFinder
	targeter    graph.UnresolvedEdgeTargetBatchReindexer
	findCalls   int
	targetCalls int
}

func newAttributionPagingProbe(store graph.Store) *attributionPagingProbe {
	return &attributionPagingProbe{
		Store:    store,
		scanner:  store.(graph.UnresolvedEdgeIdentityBatchScanner),
		finder:   store.(graph.EdgeIdentityBatchFinder),
		targeter: store.(graph.UnresolvedEdgeTargetBatchReindexer),
	}
}

func (p *attributionPagingProbe) ScanUnresolvedEdgeIdentitiesBatched(
	kinds []graph.EdgeKind,
	batchSize int,
	yield func([]graph.EdgeIdentity) bool,
) {
	p.scanner.ScanUnresolvedEdgeIdentitiesBatched(kinds, batchSize, yield)
}

func (p *attributionPagingProbe) FindEdgesByIdentities(
	identities []graph.EdgeIdentity,
) map[graph.EdgeIdentity]*graph.Edge {
	p.findCalls++
	return p.finder.FindEdgesByIdentities(identities)
}

func (p *attributionPagingProbe) ReindexUnresolvedEdgeTargets(
	batch []graph.UnresolvedEdgeTargetReindex,
) {
	p.targetCalls++
	p.targeter.ReindexUnresolvedEdgeTargets(batch)
}

func TestAttributionPagingSQLiteMatchesMemoryWithoutFullEdgeRefetch(t *testing.T) {
	memory := graph.New()
	addBareNameAttributionFixture(memory)
	New(memory).bindBareNameScopeRefs()

	sqliteStore, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqliteStore.Close()) })
	addBareNameAttributionFixture(sqliteStore)
	probe := newAttributionPagingProbe(sqliteStore)
	New(probe).bindBareNameScopeRefs()

	assert.Equal(t, 1, probe.targetCalls)
	assert.Zero(t, probe.findCalls, "unique identity-only rewrites must not decode full SQLite edge rows")
	assert.Equal(t, attributionEdgeSnapshots(memory), attributionEdgeSnapshots(sqliteStore))
}

func TestAttributionPagingConvergenceKeepsAuthoritativeFullEdgePath(t *testing.T) {
	sqliteStore, err := store_sqlite.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, sqliteStore.Close()) })
	addConvergingAttributionFixture(sqliteStore)
	probe := newAttributionPagingProbe(sqliteStore)
	New(probe).reindexAttributionEdgesBatched(
		[]graph.EdgeKind{graph.EdgeCalls},
		convergingAttributionRewrite,
	)

	assert.Zero(t, probe.targetCalls, "same-page convergence needs authoritative payload ordering")
	assert.Equal(t, 1, probe.findCalls)
	persisted := sqliteStore.GetOutEdges("repo/converge.go::Caller")
	require.Len(t, persisted, 1)
	assert.Equal(t, "repo/target.go::Shared", persisted[0].To)
	assert.Equal(t, "first", persisted[0].Origin)
}

func addBareNameAttributionFixture(store graph.Store) {
	const (
		ownerID = "repo/file.go::Func"
		localID = "repo/file.go::Func#local:x"
	)
	store.AddBatch(
		[]*graph.Node{
			{ID: ownerID, Kind: graph.KindFunction, Name: "Func", FilePath: "repo/file.go", StartLine: 1, EndLine: 20, Language: "go"},
			{ID: localID, Kind: graph.KindLocal, Name: "x", FilePath: "repo/file.go", StartLine: 4, EndLine: 4, Language: "go"},
		},
		[]*graph.Edge{
			{
				From: ownerID, To: "unresolved::x", Kind: graph.EdgeReads,
				FilePath: "repo/file.go", Line: 10,
				Confidence: 0.8, ConfidenceLabel: "heuristic", Origin: "parser", Tier: "syntax",
				Meta: map[string]any{"payload": "preserve"},
			},
			{From: ownerID, To: "unresolved::pkg.x", Kind: graph.EdgeReads, FilePath: "repo/file.go", Line: 11, Origin: "qualified"},
			{From: ownerID, To: "repo/other.go::Resolved", Kind: graph.EdgeReads, FilePath: "repo/file.go", Line: 12, Origin: "resolved"},
		},
	)
}

func addConvergingAttributionFixture(store graph.Store) {
	store.AddBatch(nil, []*graph.Edge{
		{
			From: "repo/converge.go::Caller", To: "unresolved::First", Kind: graph.EdgeCalls,
			FilePath: "repo/converge.go", Line: 7, Origin: "first", Meta: map[string]any{"winner": "first"},
		},
		{
			From: "repo/converge.go::Caller", To: "unresolved::Second", Kind: graph.EdgeCalls,
			FilePath: "repo/converge.go", Line: 7, Origin: "second", Meta: map[string]any{"winner": "second"},
		},
	})
}

func convergingAttributionRewrite(edge *graph.Edge) string {
	if edge == nil || !graph.IsUnresolvedTarget(edge.To) {
		return ""
	}
	oldTo := edge.To
	edge.To = "repo/target.go::Shared"
	return oldTo
}

type attributionEdgeSnapshot struct {
	From            string
	To              string
	Kind            graph.EdgeKind
	FilePath        string
	Line            int
	Confidence      float64
	ConfidenceLabel string
	Origin          string
	Tier            string
	Payload         string
}

func attributionEdgeSnapshots(store graph.Store) []attributionEdgeSnapshot {
	edges := store.AllEdges()
	out := make([]attributionEdgeSnapshot, 0, len(edges))
	for _, edge := range edges {
		if edge == nil {
			continue
		}
		payload, _ := edge.Meta["payload"].(string)
		if payload == "" {
			payload, _ = edge.Meta["winner"].(string)
		}
		out = append(out, attributionEdgeSnapshot{
			From: edge.From, To: edge.To, Kind: edge.Kind,
			FilePath: edge.FilePath, Line: edge.Line,
			Confidence: edge.Confidence, ConfidenceLabel: edge.ConfidenceLabel,
			Origin: edge.Origin, Tier: edge.Tier, Payload: payload,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		left, right := out[i], out[j]
		if left.From != right.From {
			return left.From < right.From
		}
		if left.To != right.To {
			return left.To < right.To
		}
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.FilePath != right.FilePath {
			return left.FilePath < right.FilePath
		}
		return left.Line < right.Line
	})
	return out
}
